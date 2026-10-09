package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/draw"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// NormalizeVisionImage decodes one still image (the first frame of a GIF), crops
// in source-image coordinates, then applies the ordinary model input limits.
func NormalizeVisionImage(ctx context.Context, data []byte, mime string, region *image.Rectangle) (NormalizationResult, error) {
	if err := ctx.Err(); err != nil {
		return NormalizationResult{}, err
	}
	if len(data) == 0 || len(data) > MaxOutputAttachmentBytes {
		return NormalizationResult{}, errors.New("vision image exceeds the source byte limit")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 8192 || config.Height > 8192 || int64(config.Width)*int64(config.Height) > 32_000_000 {
		return NormalizationResult{}, errors.New("vision image has unsupported dimensions")
	}
	expectedMIME := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}[format]
	if expectedMIME == "" || expectedMIME != mime {
		return NormalizationResult{}, errors.New("vision image type is unsupported or mismatched")
	}
	bounds := image.Rect(0, 0, config.Width, config.Height)
	if region != nil && (region.Empty() || !region.In(bounds)) {
		return NormalizationResult{}, errors.New("region must be a nonempty rectangle within the source image")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return NormalizationResult{}, errors.New("vision image could not be decoded")
	}
	if err := ctx.Err(); err != nil {
		return NormalizationResult{}, err
	}
	if region != nil {
		cropped := image.NewNRGBA(image.Rect(0, 0, region.Dx(), region.Dy()))
		draw.Draw(cropped, cropped.Bounds(), decoded, decoded.Bounds().Min.Add(region.Min), draw.Src)
		decoded = cropped
	}
	normalized, resized, encoded, normalizedMIME, err := normalizeEncodedImage(decoded, MaxNormalizedImageLongEdge, MaxNormalizedImageBytes)
	if err != nil {
		return NormalizationResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return NormalizationResult{}, err
	}
	return NormalizationResult{
		Image:        llm.InputImage{MimeType: normalizedMIME, Data: base64.StdEncoding.EncodeToString(encoded), Source: "vision_analyze"},
		DetectedMIME: mime, DecodedFormat: format,
		OriginalWidth: config.Width, OriginalHeight: config.Height,
		Width: normalized.Bounds().Dx(), Height: normalized.Bounds().Dy(),
		WasResized: resized, NormalizedBytes: len(encoded), Base64Chars: base64.StdEncoding.EncodedLen(len(encoded)),
		PreservedAlpha: hasTransparency(normalized),
	}, nil
}
