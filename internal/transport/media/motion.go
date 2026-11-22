package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxMotionPerRequest bounds motion-media fan-in per request.
	MaxMotionPerRequest = 4
	// MaxMotionBytes limits each raw inbound GIF/video payload.
	MaxMotionBytes = 25 << 20
	// MaxTotalMotionBytes bounds a whole motion admission, not each file.
	MaxTotalMotionBytes = 60 << 20
	// motionProbeTimeout bounds one best-effort ffprobe metadata read.
	motionProbeTimeout = 5 * time.Second
)

var motionExtensions = map[string]struct{}{
	".gif": {}, ".mp4": {}, ".webm": {}, ".mov": {}, ".m4v": {}, ".mkv": {},
}

// IsMotionExtension reports whether name looks like a GIF or video file.
func IsMotionExtension(name string) bool {
	_, ok := motionExtensions[strings.ToLower(strings.TrimSpace(filepath.Ext(name)))]
	return ok
}

// LooksLikeMotionMIME reports whether mimeType is a GIF or video payload.
func LooksLikeMotionMIME(mimeType string) bool {
	mimeType = normalizeMIMEType(mimeType)
	return strings.HasPrefix(mimeType, "video/") || mimeType == "image/gif"
}

// SniffMotion validates a raw GIF/video payload and returns the effective
// filename and MIME type. Declared MIME and extension decide together; the
// payload is size-checked only (operator trust boundary, like stdio commands).
func SniffMotion(name, declaredMIME string, data []byte) (string, string, error) {
	if len(data) == 0 || len(data) > MaxMotionBytes {
		return "", "", fmt.Errorf("motion media size out of bounds")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "motion"
	}
	base := filepath.Base(name)
	if base == "." || base == "/" || base == "" {
		base = "motion"
	}
	ext := strings.ToLower(filepath.Ext(base))
	mime := normalizeMIMEType(declaredMIME)
	if !LooksLikeMotionMIME(mime) {
		switch ext {
		case ".gif":
			mime = "image/gif"
		case ".mp4", ".m4v":
			mime = "video/mp4"
		case ".webm":
			mime = "video/webm"
		case ".mov":
			mime = "video/quicktime"
		case ".mkv":
			mime = "video/x-matroska"
		default:
			return "", "", fmt.Errorf("unsupported motion media type")
		}
	}
	if _, ok := motionExtensions[ext]; !ok {
		base += motionExtensionForMIME(mime)
	}
	return base, mime, nil
}

func motionExtensionForMIME(mimeType string) string {
	switch mimeType {
	case "image/gif":
		return ".gif"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "video/x-matroska":
		return ".mkv"
	default:
		return ".mp4"
	}
}

// ProbeMotion reports best-effort duration and dimensions for one motion
// file. A missing ffprobe, timeout, or unparsable output returns zeros:
// admission never fails because metadata is unavailable.
func ProbeMotion(ctx context.Context, ffprobePath, path string) (durationSeconds float64, width, height int) {
	binary := strings.TrimSpace(ffprobePath)
	if binary == "" {
		binary = "ffprobe"
	}
	probeCtx, cancel := context.WithTimeout(ctx, motionProbeTimeout)
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
