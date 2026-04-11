package media

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

const (
	// MaxImagesPerRequest limits multimodal fan-in to keep requests bounded.
	MaxImagesPerRequest = 4
	// MaxImageBytes limits each decoded image payload accepted from a gateway.
	MaxImageBytes       = 10 << 20
	imageBase64Overhead = 4
	imageBase64Quantum  = 3
)

var allowedImageMIMETypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

// SupportsMIMEType reports whether mimeType is allowed for multimodal requests.
func SupportsMIMEType(mimeType string) bool {
	_, ok := allowedImageMIMETypes[normalizeMIMEType(mimeType)]
	return ok
}

// BuildInputImage validates a raw image payload for Ollama.
func BuildInputImage(mimeType, encodedData, source string) (ollama.InputImage, error) {
	mimeType = normalizeMIMEType(mimeType)
	if !SupportsMIMEType(mimeType) {
		return ollama.InputImage{}, fmt.Errorf("unsupported image MIME type %q", mimeType)
	}
	payload := base64Payload(encodedData)
	if payload == "" {
		return ollama.InputImage{}, fmt.Errorf("image payload is empty")
	}
	if decodedLen := base64DecodedLen(payload); decodedLen <= 0 {
		return ollama.InputImage{}, fmt.Errorf("image payload is not valid base64")
	} else if decodedLen > MaxImageBytes {
		return ollama.InputImage{}, fmt.Errorf("image payload exceeds %d bytes", MaxImageBytes)
	}

	return ollama.InputImage{
		MimeType: mimeType,
		Data:     payload,
		Source:   source,
	}, nil
}

// BuildInputImageFromBytes validates and encodes raw bytes for Ollama.
func BuildInputImageFromBytes(mimeType string, data []byte, source string) (ollama.InputImage, error) {
	mimeType = normalizeMIMEType(mimeType)
	if !SupportsMIMEType(mimeType) {
		return ollama.InputImage{}, fmt.Errorf("unsupported image MIME type %q", mimeType)
	}
	if len(data) == 0 {
		return ollama.InputImage{}, fmt.Errorf("image payload is empty")
	}
	if len(data) > MaxImageBytes {
		return ollama.InputImage{}, fmt.Errorf("image payload exceeds %d bytes", MaxImageBytes)
	}

	return ollama.InputImage{
		MimeType: mimeType,
		Data:     base64.StdEncoding.EncodeToString(data),
		Source:   source,
	}, nil
}

// DetectMIMEType returns a supported image MIME type derived from the payload.
func DetectMIMEType(header http.Header, data []byte) string {
	if contentType := normalizeMIMEType(header.Get("Content-Type")); SupportsMIMEType(contentType) {
		return contentType
	}
	detected := normalizeMIMEType(http.DetectContentType(data))
	if SupportsMIMEType(detected) {
		return detected
	}
	return ""
}

func normalizeMIMEType(mimeType string) string {
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = mimeType[:idx]
	}
	return strings.TrimSpace(strings.ToLower(mimeType))
}

func base64DecodedLen(encoded string) int {
	payload := base64Payload(encoded)
	if payload == "" {
		return 0
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return 0
	}
	padding := strings.Count(payload[len(payload)-min(len(payload), 2):], "=")
	return len(payload)*imageBase64Quantum/imageBase64Overhead - padding
}

func base64Payload(encoded string) string {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" {
		return ""
	}
	if comma := strings.Index(trimmed, ","); comma >= 0 && strings.HasPrefix(trimmed[:comma], "data:") {
		return trimmed[comma+1:]
	}
	return trimmed
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
