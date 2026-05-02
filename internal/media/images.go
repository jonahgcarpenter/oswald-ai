package media

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/jpeg"
	"image/png"
	_ "image/png"
	"net/http"
	"strings"

	_ "github.com/jdeng/goheif"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	_ "golang.org/x/image/webp"
)

const (
	// MaxImagesPerRequest limits multimodal fan-in to keep requests bounded.
	MaxImagesPerRequest = 4
	// MaxImageBytes limits each decoded image payload accepted from a gateway.
	MaxImageBytes = 10 << 20
	jpegQuality   = 90
)

var normalizedImageMIMETypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
}

var decodableImageMIMETypes = map[string]struct{}{
	"image/jpeg":          {},
	"image/png":           {},
	"image/webp":          {},
	"image/heic":          {},
	"image/heif":          {},
	"image/heic-sequence": {},
	"image/heif-sequence": {},
}

// NormalizationResult describes how a raw image payload was normalized for Ollama.
type NormalizationResult struct {
	Image            ollama.InputImage
	DetectedMIME     string
	DecodedFormat    string
	Width            int
	Height           int
	PreservedAlpha   bool
	UsedDeclaredMIME bool
}

// SupportsMIMEType reports whether mimeType is allowed for multimodal requests.
func SupportsMIMEType(mimeType string) bool {
	_, ok := normalizedImageMIMETypes[normalizeMIMEType(mimeType)]
	return ok
}

// LooksLikeImageMIME reports whether mimeType looks like an image attachment.
func LooksLikeImageMIME(mimeType string) bool {
	mimeType = normalizeMIMEType(mimeType)
	return strings.HasPrefix(mimeType, "image/")
}

// BuildInputImage validates and normalizes a base64 image payload for Ollama.
func BuildInputImage(mimeType, encodedData, source string) (ollama.InputImage, error) {
	payload := base64Payload(encodedData)
	if payload == "" {
		return ollama.InputImage{}, fmt.Errorf("image payload is empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return ollama.InputImage{}, fmt.Errorf("image payload is not valid base64")
	}
	if len(decoded) > MaxImageBytes {
		return ollama.InputImage{}, fmt.Errorf("image payload exceeds %d bytes", MaxImageBytes)
	}

	result, err := NormalizeInputImageFromBytes(nil, mimeType, decoded, source)
	if err != nil {
		return ollama.InputImage{}, err
	}
	return result.Image, nil
}

// BuildInputImageFromBytes validates and normalizes raw image bytes for Ollama.
func BuildInputImageFromBytes(mimeType string, data []byte, source string) (ollama.InputImage, error) {
	result, err := NormalizeInputImageFromBytes(nil, mimeType, data, source)
	if err != nil {
		return ollama.InputImage{}, err
	}
	return result.Image, nil
}

// NormalizeInputImageFromBytes decodes a raw image, preserves alpha with PNG,
// and otherwise re-encodes to JPEG before returning the Ollama payload.
func NormalizeInputImageFromBytes(header http.Header, declaredMIME string, data []byte, source string) (NormalizationResult, error) {
	if len(data) == 0 {
		return NormalizationResult{}, fmt.Errorf("image payload is empty")
	}
	if len(data) > MaxImageBytes {
		return NormalizationResult{}, fmt.Errorf("image payload exceeds %d bytes", MaxImageBytes)
	}

	detectedMIME, usedDeclaredMIME := DetectSourceMIMEType(header, declaredMIME, data)
	if detectedMIME == "" {
		return NormalizationResult{}, fmt.Errorf("unsupported or unknown image format")
	}

	decoded, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return NormalizationResult{}, fmt.Errorf("image payload decode failed for MIME type %q: %w", detectedMIME, err)
	}

	bounds := decoded.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return NormalizationResult{}, fmt.Errorf("image payload has invalid dimensions for MIME type %q", detectedMIME)
	}

	hasAlpha := hasTransparency(decoded)
	normalizedBytes, normalizedMIME, err := encodeNormalizedImage(decoded, hasAlpha)
	if err != nil {
		return NormalizationResult{}, err
	}

	return NormalizationResult{
		Image: ollama.InputImage{
			MimeType: normalizedMIME,
			Data:     base64.StdEncoding.EncodeToString(normalizedBytes),
			Source:   source,
		},
		DetectedMIME:     detectedMIME,
		DecodedFormat:    strings.TrimSpace(strings.ToLower(format)),
		Width:            bounds.Dx(),
		Height:           bounds.Dy(),
		PreservedAlpha:   hasAlpha,
		UsedDeclaredMIME: usedDeclaredMIME,
	}, nil
}

// DetectMIMEType returns a supported image MIME type derived from the payload.
func DetectMIMEType(header http.Header, data []byte) string {
	detected, _ := DetectSourceMIMEType(header, "", data)
	return detected
}

// DetectSourceMIMEType returns a decodable image MIME type derived from bytes,
// HTTP metadata, or the declared MIME as a fallback.
func DetectSourceMIMEType(header http.Header, declaredMIME string, data []byte) (string, bool) {
	if detected := detectSpecialImageMIME(data); detected != "" {
		return detected, false
	}
	if contentType := normalizeMIMEType(header.Get("Content-Type")); supportsSourceMIMEType(contentType) {
		return contentType, false
	}
	if detected := normalizeMIMEType(http.DetectContentType(data)); supportsSourceMIMEType(detected) {
		return detected, false
	}
	declaredMIME = normalizeMIMEType(declaredMIME)
	if supportsSourceMIMEType(declaredMIME) {
		return declaredMIME, true
	}
	return "", false
}

func supportsSourceMIMEType(mimeType string) bool {
	_, ok := decodableImageMIMETypes[normalizeMIMEType(mimeType)]
	return ok
}

func detectSpecialImageMIME(data []byte) string {
	if len(data) < 12 {
		return ""
	}
	boxType := string(data[4:8])
	brand := string(data[8:12])
	if boxType != "ftyp" {
		return ""
	}
	switch brand {
	case "heic", "heix", "hevc", "hevx":
		return "image/heic"
	case "mif1", "heif", "msf1":
		return "image/heif"
	default:
		return ""
	}
}

func encodeNormalizedImage(img image.Image, preserveAlpha bool) ([]byte, string, error) {
	var buf bytes.Buffer
	if preserveAlpha {
		nrgba := toNRGBA(img)
		if err := png.Encode(&buf, nrgba); err != nil {
			return nil, "", fmt.Errorf("normalize image as PNG: %w", err)
		}
		return buf.Bytes(), "image/png", nil
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", fmt.Errorf("normalize image as JPEG: %w", err)
	}
	return buf.Bytes(), "image/jpeg", nil
}

func hasTransparency(img image.Image) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a != 0xffff {
				return true
			}
		}
	}
	return false
}

func toNRGBA(img image.Image) *image.NRGBA {
	bounds := img.Bounds()
	out := image.NewNRGBA(bounds)
	draw.Draw(out, bounds, &image.Uniform{C: color.Transparent}, image.Point{}, draw.Src)
	draw.Draw(out, bounds, img, bounds.Min, draw.Over)
	return out
}

func normalizeMIMEType(mimeType string) string {
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = mimeType[:idx]
	}
	return strings.TrimSpace(strings.ToLower(mimeType))
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
