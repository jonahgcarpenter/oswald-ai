package imagecache

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

func TestCameraOrientationMatchesCachedSourceAndVisionCrop(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 60, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 60; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(20 + y*3), A: 255})
		}
	}
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	metadata := []byte("Exif\x00\x00II\x2a\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x06\x00\x00\x00\x00\x00\x00\x00")
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(metadata)+2))
	raw := append(append(append(append([]byte(nil), jpegBytes.Bytes()[:2]...), segment...), metadata...), jpegBytes.Bytes()[2:]...)
	normalized, err := media.NormalizeInputImageFromBytes(nil, "image/jpeg", raw, "camera-roll")
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(normalized.Image.Data)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	cache := NewProfileCache(root, "alice")
	path, err := cache.Save(context.Background(), "alice", data, normalized.Image.MimeType)
	if err != nil {
		t.Fatal(err)
	}
	cached, mime, err := cache.Resolve(context.Background(), "alice", path)
	if err != nil || !bytes.Equal(cached, data) {
		t.Fatalf("cache did not preserve oriented preview bytes: %v", err)
	}
	region := image.Rect(0, 40, 20, 60)
	loaded, err := media.NormalizeVisionImage(context.Background(), cached, mime, &region)
	if err != nil || loaded.OriginalWidth != 40 || loaded.OriginalHeight != 60 || loaded.Width != 20 || loaded.Height != 20 {
		t.Fatalf("cached crop geometry does not match the displayed image: %+v err=%v", loaded, err)
	}
	preview, _ := base64.StdEncoding.DecodeString(loaded.Image.Data)
	crop, _, err := image.Decode(bytes.NewReader(preview))
	if err != nil {
		t.Fatal(err)
	}
	// Clockwise rotation maps display x=10 back to raw y=29.
	r, _, _, _ := crop.At(10, 10).RGBA()
	if value := r >> 8; value < 100 || value > 114 {
		t.Fatalf("cached crop was rotated twice or used raw coordinates: %d", value)
	}
}
