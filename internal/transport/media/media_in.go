package media

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxMediaPerRequest bounds raw inbound media fan-in per request.
	MaxMediaPerRequest = 4
	// MaxMediaBytes limits each raw inbound media payload.
	MaxMediaBytes = 25 << 20
	// MaxTotalMediaBytes bounds a whole media admission, not each file.
	MaxTotalMediaBytes = 60 << 20
	// mediaProbeTimeout bounds one best-effort ffprobe metadata read.
	mediaProbeTimeout = 5 * time.Second
)

// mediaExtensions are GIF/video/audio extensions admitted by the AV media
// lane; the agent inspects and converts them itself with ffmpeg.
var mediaExtensions = map[string]struct{}{
	".gif": {}, ".mp4": {}, ".webm": {}, ".mov": {}, ".m4v": {}, ".mkv": {},
	".mp3": {}, ".m4a": {}, ".aac": {}, ".wav": {}, ".ogg": {}, ".oga": {},
	".opus": {}, ".flac": {}, ".caf": {}, ".wma": {},
}

// imageExtensions are extension hints for the inline image lane, used to keep
// image-lane candidates out of the catch-all lane when the transport omits a
// content type.
var imageExtensions = map[string]struct{}{
	".jpg": {}, ".jpeg": {}, ".png": {}, ".webp": {}, ".heic": {}, ".heif": {},
	".bmp": {}, ".tiff": {}, ".tif": {},
}

var safeAttachmentExtRE = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)

// IsMediaExtension reports whether name looks like a GIF, video, or audio file.
func IsMediaExtension(name string) bool {
	_, ok := mediaExtensions[strings.ToLower(strings.TrimSpace(filepath.Ext(name)))]
	return ok
}

// LooksLikeImageExtension reports whether name hints at a still-image format.
func LooksLikeImageExtension(name string) bool {
	_, ok := imageExtensions[strings.ToLower(strings.TrimSpace(filepath.Ext(name)))]
	return ok
}

// LooksLikeMediaMIME reports whether mimeType is a GIF, video, or audio payload.
func LooksLikeMediaMIME(mimeType string) bool {
	mimeType = normalizeMIMEType(mimeType)
	return strings.HasPrefix(mimeType, "video/") ||
		strings.HasPrefix(mimeType, "audio/") ||
		mimeType == "image/gif"
}

// SniffMedia validates a raw GIF/video/audio payload and returns the effective
// filename and MIME type. Declared MIME and extension decide together; the
// payload is size-checked only (operator trust boundary, like stdio commands).
func SniffMedia(name, declaredMIME string, data []byte) (string, string, error) {
	if len(data) == 0 || len(data) > MaxMediaBytes {
		return "", "", fmt.Errorf("media size out of bounds")
	}
	base := safeBaseName(name, "media")
	ext := strings.ToLower(filepath.Ext(base))
	mime := normalizeMIMEType(declaredMIME)
	if !LooksLikeMediaMIME(mime) {
		mapped, ok := mediaMIMEForExtension(ext)
		if !ok {
			return "", "", fmt.Errorf("unsupported media type")
		}
		mime = mapped
	}
	if _, ok := mediaExtensions[ext]; !ok {
		base += mediaExtensionForMIME(mime)
	}
	return base, mime, nil
}

// SniffAttachment prepares any other bounded attachment for the catch-all raw
// lane: a safe filename extension is kept, and the MIME type comes from the
// declaration or content sniffing. The agent decides what to do with it.
func SniffAttachment(name, declaredMIME string, data []byte) (string, string, error) {
	if len(data) == 0 || len(data) > MaxMediaBytes {
		return "", "", fmt.Errorf("attachment size out of bounds")
	}
	base := safeBaseName(name, "attachment")
	ext := strings.ToLower(filepath.Ext(base))
	if ext != "" && !safeAttachmentExtRE.MatchString(ext) {
		base = strings.TrimSuffix(base, filepath.Ext(base))
		if base == "" {
			base = "attachment"
		}
	}
	mime := normalizeMIMEType(declaredMIME)
	if mime == "" || mime == "application/octet-stream" {
		mime = normalizeMIMEType(http.DetectContentType(data))
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return base, mime, nil
}

func safeBaseName(name, fallback string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = fallback
	}
	base := filepath.Base(name)
	if base == "." || base == "/" || base == "" {
		return fallback
	}
	return base
}

func mediaMIMEForExtension(ext string) (string, bool) {
	switch ext {
	case ".gif":
		return "image/gif", true
	case ".mp4", ".m4v":
		return "video/mp4", true
	case ".webm":
		return "video/webm", true
	case ".mov":
		return "video/quicktime", true
	case ".mkv":
		return "video/x-matroska", true
	case ".mp3":
		return "audio/mpeg", true
	case ".m4a", ".caf":
		return "audio/mp4", true
	case ".aac":
		return "audio/aac", true
	case ".wav":
		return "audio/wav", true
	case ".ogg", ".oga":
		return "audio/ogg", true
	case ".opus":
		return "audio/opus", true
	case ".flac":
		return "audio/flac", true
	case ".wma":
		return "audio/x-ms-wma", true
	default:
		return "", false
	}
}

func mediaExtensionForMIME(mimeType string) string {
	switch mimeType {
	case "image/gif":
		return ".gif"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "video/x-matroska":
		return ".mkv"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	case "audio/aac":
		return ".aac"
	case "audio/wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "audio/opus":
		return ".opus"
	case "audio/flac":
		return ".flac"
	case "audio/x-ms-wma":
		return ".wma"
	default:
		return ".mp4"
	}
}

// OutputMediaMIME maps a filename extension accepted for MEDIA: delivery to
// its media MIME type. It covers still images, GIF, video, and audio so the
// model can attach any media type from a cache path or workspace path.
func OutputMediaMIME(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(filepath.Ext(name))) {
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	case ".bmp":
		return "image/bmp", true
	case ".tif", ".tiff":
		return "image/tiff", true
	case ".heic":
		return "image/heic", true
	case ".heif":
		return "image/heif", true
	case ".mp4", ".m4v":
		return "video/mp4", true
	case ".webm":
		return "video/webm", true
	case ".mov":
		return "video/quicktime", true
	case ".mkv":
		return "video/x-matroska", true
	case ".mp3":
		return "audio/mpeg", true
	case ".m4a":
		return "audio/mp4", true
	case ".aac":
		return "audio/aac", true
	case ".wav":
		return "audio/wav", true
	case ".ogg", ".oga":
		return "audio/ogg", true
	case ".opus":
		return "audio/opus", true
	case ".flac":
		return "audio/flac", true
	case ".caf":
		return "audio/x-caf", true
	case ".wma":
		return "audio/x-ms-wma", true
	default:
		return "", false
	}
}

// ProbeMedia reports best-effort duration and dimensions for one media file.
// Audio yields duration with zero dimensions. A missing ffprobe, timeout, or
// unparsable output returns zeros: admission never fails for missing metadata.
func ProbeMedia(ctx context.Context, ffprobePath, path string) (durationSeconds float64, width, height int) {
	binary := strings.TrimSpace(ffprobePath)
	if binary == "" {
		binary = "ffprobe"
	}
	probeCtx, cancel := context.WithTimeout(ctx, mediaProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, binary,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-show_entries", "format=duration",
		"-of", "json",
		path,
	).Output()
	if err != nil {
		return 0, 0, 0
	}
	var result struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return 0, 0, 0
	}
	if len(result.Streams) > 0 {
		width = result.Streams[0].Width
		height = result.Streams[0].Height
	}
	if duration, parseErr := strconv.ParseFloat(strings.TrimSpace(result.Format.Duration), 64); parseErr == nil && duration > 0 {
		durationSeconds = duration
	}
	return durationSeconds, width, height
}
