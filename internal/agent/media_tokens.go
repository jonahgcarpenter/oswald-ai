package agent

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	_ "golang.org/x/image/webp"
	"golang.org/x/sys/unix"
)

// mediaTokenPattern matches MEDIA:/absolute/path tokens in final model text.
// Only absolute paths match; relative paths and bare words are left alone.
var mediaTokenPattern = regexp.MustCompile(`MEDIA:(/\S+)`)

// collapseStrippedWhitespace tidies text left behind by removed MEDIA tokens
// without reflowing the model's own paragraphs.
func collapseStrippedWhitespace(content string) string {
	collapsed := regexp.MustCompile(`[ \t]{2,}`).ReplaceAllString(content, " ")
	collapsed = regexp.MustCompile(`\n{3,}`).ReplaceAllString(collapsed, "\n\n")
	return strings.TrimSpace(collapsed)
}

// trimTokenPunctuation strips sentence punctuation a model may append directly
// after a path (for example a trailing period). Managed cache names never end
// in these characters, so trimming cannot alias a different cached file.
func trimTokenPunctuation(path string) string {
	return strings.TrimRight(path, `.,;:!?)]}'"`)
}

// resolveResponseMedia extracts MEDIA:/absolute/path tokens from final model
// text, resolves each against the sender's unexpired managed image cache
// first, then against raw absolute paths inside the profile working
// directory, and returns stripped text plus deliverable attachments. Tokens
// are delivery directives, not message content: they never survive into
// stored history.
//
// Cache resolution is confined by imagecache.Resolve to regular, private,
// unexpired cache images owned by senderID. The CWD fallback is confined by
// resolveCWDImage to regular image files under workspaceRoot with
// containment, symlink, secret, size, and MIME enforcement. Paths outside
// both fail closed and are stripped text-only. The text-only API gateway
// strips tokens without attaching. Combined tool plus media attachments stay
// within media.ValidateOutputAttachments; overflow and filename collisions
// are dropped.
func (a *Agent) resolveResponseMedia(ctx context.Context, log *config.Logger, gateway, senderID, content string, existing []media.OutputAttachment) (string, []media.OutputAttachment) {
	matches := mediaTokenPattern.FindAllStringSubmatch(content, -1)
	stripped := collapseStrippedWhitespace(mediaTokenPattern.ReplaceAllString(content, ""))
	if len(matches) == 0 || gateway == "openai" || (a.imageCache == nil && strings.TrimSpace(a.workspaceRoot) == "") {
		return stripped, nil
	}
	seen := make(map[string]bool, len(matches))
	var attached []media.OutputAttachment
	dropped := 0
	for _, match := range matches {
		path := trimTokenPunctuation(match[1])
		if seen[path] {
			continue
		}
		seen[path] = true
		var data []byte
		var mimeType string
		if a.imageCache != nil {
			data, mimeType, _ = a.imageCache.Resolve(ctx, senderID, path)
		}
		if data == nil && strings.TrimSpace(a.workspaceRoot) != "" {
			data, mimeType, _ = a.resolveCWDImage(ctx, path)
		}
		if data == nil {
			dropped++
			continue
		}
		candidate := media.OutputAttachment{Filename: filepath.Base(path), MIMEType: mimeType, Data: data}
		combined := make([]media.OutputAttachment, 0, len(existing)+len(attached)+1)
		combined = append(combined, existing...)
		combined = append(combined, attached...)
		combined = append(combined, candidate)
		if err := media.ValidateOutputAttachments(combined); err != nil {
			dropped++
			continue
		}
		attached = append(attached, candidate)
	}
	if log != nil {
		log.Debug("agent.response.media", "resolved MEDIA tokens from model response", config.F("attached_count", len(attached)), config.F("dropped_count", dropped))
	}
	return stripped, attached
}

// resolveCWDImage reads a regular image file at an absolute path confined
// inside the profile working directory. Managed cache paths must go through
// imagecache.Resolve (which enforces ownership and expiry); this fallback
// denies cache siblings except cache/scratch so expiry semantics are
// preserved, and denies operator secrets, state, memory, tokens, and
// cross-profile paths mirroring the workspace sandbox.
func (a *Agent) resolveCWDImage(ctx context.Context, path string) ([]byte, string, error) {
	root := strings.TrimSpace(a.workspaceRoot)
	if root == "" {
		return nil, "", errNoWorkspace
	}
	if !filepath.IsAbs(path) {
		return nil, "", errOutsideWorkspace
	}
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(root, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, "", errOutsideWorkspace
	}
	if deniedCWDPath(rel) {
		return nil, "", errDeniedWorkspacePath
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return nil, "", errOutsideWorkspace
	}
	resolved = filepath.Clean(resolved)
	rel, err = filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, "", errOutsideWorkspace
	}
	if deniedCWDPath(rel) {
		return nil, "", errDeniedWorkspacePath
	}
	if info, err := os.Lstat(resolved); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, "", errOutsideWorkspace
	}
	fd, err := unix.Open(resolved, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", errOutsideWorkspace
	}
	f := os.NewFile(uintptr(fd), resolved)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, "", errOutsideWorkspace
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > media.MaxOutputAttachmentBytes {
		f.Close()
		return nil, "", errImageOutOfBounds
	}
	if err := ctx.Err(); err != nil {
		f.Close()
		return nil, "", err
	}
	data, err := readLimited(f, media.MaxOutputAttachmentBytes+1)
	f.Close()
	if err != nil {
		return nil, "", errOutsideWorkspace
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	mimeType, err := validateCWDImage(data, resolved)
	if err != nil {
		return nil, "", err
	}
	return data, mimeType, nil
}

var (
	errNoWorkspace         = errors.New("workspace is unavailable")
	errOutsideWorkspace    = errors.New("path escapes the working directory")
	errDeniedWorkspacePath = errors.New("path is not accessible")
	errImageOutOfBounds    = errors.New("image size out of bounds")
	errUnsupportedImage    = errors.New("unsupported image signature")
)

// deniedCWDPath mirrors the workspace sandbox denies relevant to MEDIA
// delivery: operator secrets, application state, managed memory, tokens,
// cross-profile subtrees, and managed cache except the scratch area (which
// stays readable so model-produced temp images can attach; cache/images
// must go through the cache resolver with ownership and expiry).
func deniedCWDPath(rel string) bool {
	parts := strings.Split(rel, string(filepath.Separator))
	first := ""
	if len(parts) > 0 {
		first = parts[0]
	}
	base := filepath.Base(rel)
	switch base {
	case "state.db", "state.db-wal", "state.db-shm", "state.db-journal", "state.db-journal-mode",
		"config.yaml", ".env", "SOUL.md":
		return true
	}
	if strings.HasPrefix(base, ".env") {
		return true
	}
	if base == "credentials" || strings.HasPrefix(base, "credentials.") ||
		strings.HasSuffix(base, "credentials.json") ||
		strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
		return true
	}
	switch first {
	case "mcp-tokens", "memories", "profiles":
		return true
	}
	if first == "cache" {
		if rel == "cache/scratch" || strings.HasPrefix(rel, "cache/scratch"+string(filepath.Separator)) {
			return false
		}
		return true
	}
	return false
}

// readLimited reads f up to limit bytes; callers size-check before trusting.
func readLimited(f *os.File, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// validateCWDImage enforces the managed-cache image bounds (8 MiB, valid
// still-image signature, dimensions within 8192) and requires the file
// extension to match the detected content, case-insensitively.
func validateCWDImage(data []byte, path string) (string, error) {
	if len(data) == 0 || len(data) > media.MaxOutputAttachmentBytes {
		return "", errImageOutOfBounds
	}
	mimeType := http.DetectContentType(data)
	ext := ""
	switch mimeType {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	default:
		return "", errUnsupportedImage
	}
	fileExt := strings.ToLower(filepath.Ext(path))
	if fileExt == ".jpeg" {
		fileExt = ".jpg"
	}
	if fileExt != ext {
		return "", errUnsupportedImage
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 8192 || cfg.Height > 8192 {
		return "", errUnsupportedImage
	}
	if format != map[string]string{".png": "png", ".jpg": "jpeg", ".gif": "gif", ".webp": "webp"}[ext] {
		return "", errUnsupportedImage
	}
	return mimeType, nil
}
