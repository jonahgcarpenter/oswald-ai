package media

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/gif"
	"image/png"
	"testing"
)

func TestSearchPreviewNormalizationAndPredecodeBounds(t *testing.T) {
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewNRGBA(image.Rect(0, 0, 20, 10))); err != nil {
		t.Fatal(err)
	}
	preview, err := NormalizeSearchPreview(pngData.Bytes(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(preview.Data)
	if err != nil || len(data) > MaxNormalizedImageBytes || preview.MimeType != "image/png" {
		t.Fatal("invalid normalization")
	}
	var large bytes.Buffer
	if err := png.Encode(&large, image.NewGray(image.Rect(0, 0, 4097, 1))); err != nil {
		t.Fatal(err)
	}
	var animation bytes.Buffer
	if err := gif.Encode(&animation, image.NewGray(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		data []byte
		mime string
	}{
		{pngData.Bytes(), "image/jpeg"}, {large.Bytes(), "image/png"}, {animation.Bytes(), "image/gif"},
		{[]byte("<svg/>"), "image/svg+xml"}, {make([]byte, MaxSearchPreviewBytes+1), "image/png"},
	} {
		if _, err := NormalizeSearchPreview(tc.data, tc.mime); err == nil {
			t.Fatal("accepted unsafe preview")
		}
	}
}
