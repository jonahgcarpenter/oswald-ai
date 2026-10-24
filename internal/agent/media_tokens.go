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
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
	_ "golang.org/x/image/webp"
	"golang.org/x/sys/unix"
)

// mediaTokenPattern matches MEDIA:/absolute/path tokens in final model text.
// Only absolute paths match; relative paths and bare words are left alone.
var mediaTokenPattern = regexp.MustCompile(`MEDIA:(/\S+)`)

// fileTokenPattern matches FILE:/absolute/path tokens in final model text.
// Same absolute-path rule as MEDIA; generic files of any type accepted.
var fileTokenPattern = regexp.MustCompile(`FILE:(/\S+)`)

// responseTokenPattern matches both delivery directives in textual order so
// shared attachment caps and dedupe respect the model's ordering.
var responseTokenPattern = regexp.MustCompile(`(MEDIA|FILE):(/\S+)`)

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

// resolveResponseMedia extracts MEDIA:/absolute/path and FILE:/absolute/path
// tokens from final model text, resolves each against the sender's unexpired
// managed image cache first (MEDIA only), then against raw absolute paths
// inside the profile working directory, and returns stripped text plus
// deliverable attachments. Tokens are delivery directives, not message
// content: they never survive into stored history.
//
// Cache resolution is confined by imagecache.Resolve to regular, private,
// unexpired cache images owned by senderID. The CWD fallback resolves raw
// absolute paths under the file groups (protected names sealed, managed
// cache via the cache resolver) with symlink, secret, and size enforcement:
// MEDIA additionally requires a still-image signature with matching
// extension, while FILE accepts any file type via content sniffing with an
// extension fallback. Paths resolving nowhere fail closed and are stripped
// text-only. The text-only API gateway
// strips tokens without attaching. Combined tool plus media attachments stay
// within media.ValidateOutputAttachments; overflow and filename collisions
// are dropped. Dedupe is shared by absolute path across both directives.
func (a *Agent) resolveResponseMedia(ctx context.Context, log *config.Logger, gateway, senderID, content string, existing []media.OutputAttachment) (string, []media.OutputAttachment) {
	ordered := responseTokenPattern.FindAllStringSubmatch(content, -1)
	stripped := collapseStrippedWhitespace(fileTokenPattern.ReplaceAllString(mediaTokenPattern.ReplaceAllString(content, ""), ""))
	if len(ordered) == 0 || gateway == "openai" || (a.imageCache == nil && strings.TrimSpace(a.workspaceRoot) == "") {
		return stripped, nil
	}
	seen := make(map[string]bool, len(ordered))
	var attached []media.OutputAttachment
	dropped := 0
	fileAttached := 0
	for _, match := range ordered {
		kind := match[1]
		path := trimTokenPunctuation(match[2])
		if seen[path] {
			continue
		}
		seen[path] = true
		var data []byte
		var mimeType string
		if kind == "MEDIA" {
			if a.imageCache != nil {
				data, mimeType, _ = a.imageCache.Resolve(ctx, senderID, path)
			}
			if data == nil && strings.TrimSpace(a.workspaceRoot) != "" {
				data, mimeType, _ = a.resolveCWDImage(ctx, path)
			}
		} else {
			if strings.TrimSpace(a.workspaceRoot) != "" {
				data, mimeType, _ = a.resolveCWDFile(ctx, path)
			}
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
		if kind == "FILE" {
			fileAttached++
		}
	}
	if log != nil {
		log.Debug("agent.response.media", "resolved delivery tokens from model response", config.F("attached_count", len(attached)), config.F("dropped_count", dropped), config.F("file_attached_count", fileAttached))
	}
	return stripped, attached
}

// resolveCWDImage reads a regular image file at an absolute path. Managed
// cache paths must go through imagecache.Resolve (which enforces ownership
// and expiry); this fallback denies cache siblings except cache/scratch so
// expiry semantics are preserved, and denies protected names through the
// shared file groups. Everything else resolves under the same-trust model:
// system and agent paths (including outside the workspace) may attach,
// subject to image validation below.
func (a *Agent) resolveCWDImage(ctx context.Context, path string) ([]byte, string, error) {
	data, resolved, err := a.openCWDRegularFile(ctx, path)
	if err != nil {
		return nil, "", err
	}
	mimeType, err := validateCWDImage(data, resolved)
	if err != nil {
		return nil, "", err
	}
	return data, mimeType, nil
}

// resolveCWDFile reads a regular file of any type at an absolute path for
// FILE: delivery. It enforces the same confinement as resolveCWDImage
// (protected names sealed, managed cache via the cache resolver, symlink and
// size enforcement) but accepts any file type: the MIME type is sniffed from
// content with an extension fallback, defaulting to
// application/octet-stream.
func (a *Agent) resolveCWDFile(ctx context.Context, path string) ([]byte, string, error) {
	data, resolved, err := a.openCWDRegularFile(ctx, path)
	if err != nil {
		return nil, "", err
	}
	mimeType, err := sniffCWDFileMIME(data, resolved)
	if err != nil {
		return nil, "", err
	}
	return data, mimeType, nil
}

// openCWDRegularFile securely opens a regular file under the same-trust
// model shared by MEDIA and FILE delivery. It denies protected names and
// managed cache paths (except cache/scratch) before and after symlink
// resolution, opens with O_NOFOLLOW, and returns at most
// MaxOutputAttachmentBytes+1 bytes for caller size checks.
func (a *Agent) openCWDRegularFile(ctx context.Context, path string) ([]byte, string, error) {
	root := strings.TrimSpace(a.workspaceRoot)
	if root == "" {
		return nil, "", errNoWorkspace
	}
	if !filepath.IsAbs(path) {
		return nil, "", errOutsideWorkspace
	}
	clean := filepath.Clean(path)
	if workspace.Classify(clean, root) == workspace.GroupProtected {
		return nil, "", errDeniedWorkspacePath
	}
	if inManagedCache(clean, root) {
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
	if workspace.Classify(resolved, root) == workspace.GroupProtected {
		return nil, "", errDeniedWorkspacePath
	}
	if inManagedCache(resolved, root) {
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
		if info.Size() == 0 || info.Size() > media.MaxOutputAttachmentBytes {
			return nil, "", errFileOutOfBounds
		}
		return nil, "", errOutsideWorkspace
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
	return data, resolved, nil
}

var (
	errNoWorkspace         = errors.New("workspace is unavailable")
	errOutsideWorkspace    = errors.New("path escapes the working directory")
	errDeniedWorkspacePath = errors.New("path is not accessible")
	errImageOutOfBounds    = errors.New("image size out of bounds")
	errFileOutOfBounds     = errors.New("file size out of bounds")
	errUnsupportedImage    = errors.New("unsupported image signature")
)

// inManagedCache reports workspace cache paths that must resolve through the
// managed image cache (ownership and expiry) instead of the raw CWD
// fallback. cache/scratch stays readable so model-produced temp images can
// attach.
func inManagedCache(abs, root string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) > 0 && parts[0] == "cache" &&
		rel != "cache" && rel != "cache/scratch" && !strings.HasPrefix(rel, "cache/scratch"+string(filepath.Separator))
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

// sniffCWDFileMIME returns a transport-valid MIME type for generic FILE
// delivery. Content sniffing wins; when it is generic (octet-stream or
// text/plain) an extension lookup refines it. The result always parses as
// type/subtype, defaulting to application/octet-stream.
func sniffCWDFileMIME(data []byte, path string) (string, error) {
	if len(data) == 0 || len(data) > media.MaxOutputAttachmentBytes {
		return "", errFileOutOfBounds
	}
	sniffed := http.DetectContentType(data)
	if sniffed == "application/octet-stream" || strings.HasPrefix(sniffed, "text/plain") {
		if ext := strings.ToLower(strings.TrimSpace(filepath.Ext(path))); ext != "" {
			if byExt := strings.TrimSpace(mime.TypeByExtension(ext)); byExt != "" {
				if mt, _, err := mime.ParseMediaType(byExt); err == nil && strings.Contains(mt, "/") {
					sniffed = byExt
				}
			}
		}
	}
	if mt, _, err := mime.ParseMediaType(strings.TrimSpace(sniffed)); err != nil || !strings.Contains(mt, "/") {
		return "application/octet-stream", nil
	}
	return strings.TrimSpace(sniffed), nil
}
