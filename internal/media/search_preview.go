package media

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"mime"
	"net/http"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// MaxSearchPreviewBytes bounds downloaded thumbnails before decoding.
const MaxSearchPreviewBytes = 2 << 20

// NormalizeSearchPreview accepts only JPEG/PNG/WebP and validates dimensions
// before full decode. GIF/HEIF/SVG and animation expansion are deliberately absent.
// Returned normalized bytes are the sole source for both vision and delivery.
func NormalizeSearchPreview(data []byte, contentType string) (llm.InputImage, error) {
	if len(data) == 0 || len(data) > MaxSearchPreviewBytes {
		return llm.InputImage{}, errors.New("thumbnail size limit")
	}
	detected := http.DetectContentType(data)
	if detected != "image/jpeg" && detected != "image/png" && detected != "image/webp" {
		return llm.InputImage{}, errors.New("unsupported thumbnail format")
	}
	if contentType != "" {
		declared, _, err := mime.ParseMediaType(contentType)
		if err != nil || declared != detected {
			return llm.InputImage{}, errors.New("thumbnail MIME mismatch")
		}
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png" && format != "webp") || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 || int64(cfg.Width)*int64(cfg.Height) > 4_000_000 {
		return llm.InputImage{}, errors.New("thumbnail dimensions or format rejected")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return llm.InputImage{}, errors.New("thumbnail decode failed")
	}
	_, _, normalized, mimeType, err := normalizeEncodedImage(decoded, 1024, MaxNormalizedImageBytes)
	if err != nil {
		return llm.InputImage{}, err
	}
	return llm.InputImage{MimeType: mimeType, Data: base64.StdEncoding.EncodeToString(normalized), Source: "web.image_search"}, nil
}
