package media

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	imagedraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/jpeg"
	"image/png"
	_ "image/png"
	"net/http"
	"strings"

	_ "github.com/jdeng/goheif"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	// MaxImagesPerRequest limits multimodal fan-in to keep requests bounded.
	MaxImagesPerRequest = 4
	// MaxImageBytes limits each decoded image payload accepted from a gateway.
	MaxImageBytes = 10 << 20
	// MaxNormalizedImageLongEdge limits the pixels sent to vision models while preserving useful detail.
	MaxNormalizedImageLongEdge = 2560
	jpegQuality                = 90
)

var normalizedImageMIMETypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
}

var decodableImageMIMETypes = map[string]struct{}{
	"image/jpeg":          {},
	"image/png":           {},
	"image/gif":           {},
	"image/webp":          {},
	"image/heic":          {},
	"image/heif":          {},
	"image/heic-sequence": {},
	"image/heif-sequence": {},
}

// NormalizationResult describes how a raw image payload was normalized for the LLM provider.
type NormalizationResult struct {
	Image            llm.InputImage
	DetectedMIME     string
	DecodedFormat    string
	OriginalWidth    int
	OriginalHeight   int
	Width            int
	Height           int
	WasResized       bool
	NormalizedBytes  int
	Base64Chars      int
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

// BuildInputImage validates and normalizes a base64 image payload for the LLM provider.
func BuildInputImage(mimeType, encodedData, source string) (llm.InputImage, error) {
	payload := base64Payload(encodedData)
	if payload == "" {
		return llm.InputImage{}, fmt.Errorf("image payload is empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return llm.InputImage{}, fmt.Errorf("image payload is not valid base64")
	}
	if len(decoded) > MaxImageBytes {
		return llm.InputImage{}, fmt.Errorf("image payload exceeds %d bytes", MaxImageBytes)
	}

	result, err := NormalizeInputImageFromBytes(nil, mimeType, decoded, source)
	if err != nil {
		return llm.InputImage{}, err
	}
	return result.Image, nil
}

// BuildInputImageFromBytes validates and normalizes raw image bytes for the LLM provider.
func BuildInputImageFromBytes(mimeType string, data []byte, source string) (llm.InputImage, error) {
	result, err := NormalizeInputImageFromBytes(nil, mimeType, data, source)
	if err != nil {
		return llm.InputImage{}, err
	}
	return result.Image, nil
}

// NormalizeInputImageFromBytes decodes a raw image, preserves alpha with PNG,
// and otherwise re-encodes to JPEG before returning the LLM payload.
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

	originalBounds := decoded.Bounds()
	originalWidth := originalBounds.Dx()
	originalHeight := originalBounds.Dy()
	if originalWidth <= 0 || originalHeight <= 0 {
		return NormalizationResult{}, fmt.Errorf("image payload has invalid dimensions for MIME type %q", detectedMIME)
	}

	normalizedImage, wasResized := resizeToFit(decoded, MaxNormalizedImageLongEdge)
	normalizedBounds := normalizedImage.Bounds()
	normalizedWidth := normalizedBounds.Dx()
	normalizedHeight := normalizedBounds.Dy()
	hasAlpha := hasTransparency(normalizedImage)
	normalizedBytes, normalizedMIME, err := encodeNormalizedImage(normalizedImage, hasAlpha)
	if err != nil {
		return NormalizationResult{}, err
	}
	encoded := base64.StdEncoding.EncodeToString(normalizedBytes)

	return NormalizationResult{
		Image: llm.InputImage{
			MimeType: normalizedMIME,
			Data:     encoded,
			Source:   source,
		},
		DetectedMIME:     detectedMIME,
		DecodedFormat:    strings.TrimSpace(strings.ToLower(format)),
		OriginalWidth:    originalWidth,
		OriginalHeight:   originalHeight,
		Width:            normalizedWidth,
		Height:           normalizedHeight,
		WasResized:       wasResized,
		NormalizedBytes:  len(normalizedBytes),
		Base64Chars:      len(encoded),
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

func resizeToFit(img image.Image, maxLongEdge int) (image.Image, bool) {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if maxLongEdge <= 0 || width <= maxLongEdge && height <= maxLongEdge {
		return img, false
	}

	scale := float64(maxLongEdge) / float64(width)
	if height > width {
		scale = float64(maxLongEdge) / float64(height)
	}
	newWidth := max(1, int(float64(width)*scale+0.5))
	newHeight := max(1, int(float64(height)*scale+0.5))
	resized := image.NewNRGBA(image.Rect(0, 0, newWidth, newHeight))
	xdraw.CatmullRom.Scale(resized, resized.Bounds(), img, bounds, xdraw.Over, nil)
	return resized, true
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
	imagedraw.Draw(out, bounds, &image.Uniform{C: color.Transparent}, image.Point{}, imagedraw.Src)
	imagedraw.Draw(out, bounds, img, bounds.Min, imagedraw.Over)
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
