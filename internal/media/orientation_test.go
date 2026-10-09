package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/jdeng/goheif/heif/bmff"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

func orientationTIFF(orientation uint16, order binary.ByteOrder) []byte {
	data := make([]byte, 26)
	copy(data, "II")
	if order == binary.BigEndian {
		copy(data, "MM")
	}
	order.PutUint16(data[2:4], 42)
	order.PutUint32(data[4:8], 8)
	order.PutUint16(data[8:10], 1)
	order.PutUint16(data[10:12], 0x0112)
	order.PutUint16(data[12:14], 3)
	order.PutUint32(data[14:18], 1)
	order.PutUint16(data[18:20], orientation)
	return data
}

func jpegWithOrientation(t *testing.T, img image.Image, orientation uint16, order binary.ByteOrder) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	metadata := append([]byte("Exif\x00\x00"), orientationTIFF(orientation, order)...)
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(metadata)+2))
	data := append([]byte(nil), encoded.Bytes()[:2]...)
	data = append(data, segment...)
	data = append(data, metadata...)
	return append(data, encoded.Bytes()[2:]...)
}

func orientationGrid() *image.NRGBA {
	// Nonzero bounds catch transforms that incorrectly assume origin (0,0).
	img := image.NewNRGBA(image.Rect(7, 9, 10, 11))
	for i := 0; i < 6; i++ {
		img.SetNRGBA(7+i%3, 9+i/3, color.NRGBA{R: uint8(i + 1), A: uint8(200 + i)})
	}
	return img
}

func checkOrientationGrid(t *testing.T, img image.Image, want []byte, width, height int) {
	t.Helper()
	if img.Bounds().Dx() != width || img.Bounds().Dy() != height {
		t.Fatalf("dimensions=%v want=%dx%d", img.Bounds(), width, height)
	}
	for i, value := range want {
		pixel := color.NRGBAModel.Convert(img.At(img.Bounds().Min.X+i%width, img.Bounds().Min.Y+i/width)).(color.NRGBA)
		if pixel.R != value || pixel.A != 199+value {
			t.Fatalf("pixel %d=%+v want label=%d with alpha preserved", i, pixel, value)
		}
	}
}

func TestAllEightDisplayOrientations(t *testing.T) {
	for _, test := range []struct {
		orientation   int
		width, height int
		pixels        []byte
	}{
		{1, 3, 2, []byte{1, 2, 3, 4, 5, 6}},
		{2, 3, 2, []byte{3, 2, 1, 6, 5, 4}},
		{3, 3, 2, []byte{6, 5, 4, 3, 2, 1}},
		{4, 3, 2, []byte{4, 5, 6, 1, 2, 3}},
		{5, 2, 3, []byte{1, 4, 2, 5, 3, 6}},
		{6, 2, 3, []byte{4, 1, 5, 2, 6, 3}},
		{7, 2, 3, []byte{6, 3, 5, 2, 4, 1}},
		{8, 2, 3, []byte{3, 6, 2, 5, 1, 4}},
	} {
		t.Run(fmt.Sprint(test.orientation), func(t *testing.T) {
			for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
				value, ok := tiffOrientation(orientationTIFF(uint16(test.orientation), order))
				if !ok || value != test.orientation {
					t.Fatal("TIFF orientation not recognized")
				}
				checkOrientationGrid(t, applyImageOrientation(orientationGrid(), value), test.pixels, test.width, test.height)
			}
		})
	}
}

func TestJPEGOrientationBeforeNormalizationCropAndRetries(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 60, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 60; x++ {
			value := uint8(20 + 30*(x/20+3*(y/20)))
			img.SetNRGBA(x, y, color.NRGBA{R: value, G: value, B: value, A: 255})
		}
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		source := jpegWithOrientation(t, img, 6, order)
		result, err := NormalizeInputImageFromBytes(nil, "image/jpeg", source, "camera-roll")
		if err != nil {
			t.Fatal(err)
		}
		if result.OriginalWidth != 40 || result.OriginalHeight != 60 || result.Width != 40 || result.Height != 60 || result.WasResized {
			t.Fatalf("orientation confused with resizing: %+v", result)
		}
		data, err := base64.StdEncoding.DecodeString(result.Image.Data)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := tiffOrientation(data); ok || sourceEXIFOrientation(data, "jpeg") != 1 {
			t.Fatal("normalized image retained rotation metadata")
		}
		decoded, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if pixel := color.NRGBAModel.Convert(decoded.At(10, 10)).(color.NRGBA); pixel.R < 105 || pixel.R > 115 {
			t.Fatalf("preview was not rotated clockwise: %+v", pixel)
		}
		cached, err := NormalizeInputImageFromBytes(nil, result.Image.MimeType, data, "cached")
		if err != nil || cached.Width != 40 || cached.Height != 60 {
			t.Fatal("normalized cached image rotated twice")
		}
		region := image.Rect(20, 40, 40, 60) // Valid only in the oriented dimensions.
		cropped, err := NormalizeVisionImage(context.Background(), source, "image/jpeg", &region)
		if err != nil || cropped.OriginalWidth != 40 || cropped.OriginalHeight != 60 || cropped.Width != 20 || cropped.Height != 20 {
			t.Fatalf("crop did not use oriented source coordinates: %+v err=%v", cropped, err)
		}
		cropBytes, _ := base64.StdEncoding.DecodeString(cropped.Image.Data)
		cropImage, _, err := image.Decode(bytes.NewReader(cropBytes))
		if err != nil {
			t.Fatal(err)
		}
		if pixel := color.NRGBAModel.Convert(cropImage.At(10, 10)).(color.NRGBA); pixel.R < 75 || pixel.R > 85 {
			t.Fatalf("crop selected wrong oriented pixel: %+v", pixel)
		}
		invalid := image.Rect(40, 0, 60, 20) // Raw sensor bounds, not display bounds.
		if _, err := NormalizeVisionImage(context.Background(), source, "image/jpeg", &invalid); err == nil {
			t.Fatal("crop accepted unrotated coordinates")
		}
		retried, err := ResizeInputImagesForAttempt([]llm.InputImage{result.Image}, 2, 0.5, MaxNormalizedImageLongEdge)
		if err != nil || len(retried) != 1 {
			t.Fatalf("retry: %v", err)
		}
		retryBytes, _ := base64.StdEncoding.DecodeString(retried[0].Data)
		config, _, err := image.DecodeConfig(bytes.NewReader(retryBytes))
		if err != nil || config.Width != 20 || config.Height != 30 {
			t.Fatal("retry changed display orientation")
		}
	}
}

func TestHEIFRotationMirrorAndAssociationOrder(t *testing.T) {
	for _, test := range []struct {
		properties    []bmff.Box
		width, height int
		pixels        []byte
	}{
		{nil, 3, 2, []byte{1, 2, 3, 4, 5, 6}},
		{[]bmff.Box{&bmff.ImageRotation{Angle: 1}}, 2, 3, []byte{3, 6, 2, 5, 1, 4}},
		{[]bmff.Box{&bmff.ImageRotation{Angle: 2}}, 3, 2, []byte{6, 5, 4, 3, 2, 1}},
		{[]bmff.Box{&bmff.ImageRotation{Angle: 3}}, 2, 3, []byte{4, 1, 5, 2, 6, 3}},
		{[]bmff.Box{&bmff.ImageMirror{Mirror: bmff.MirrorVertical}}, 3, 2, []byte{3, 2, 1, 6, 5, 4}},
		{[]bmff.Box{&bmff.ImageMirror{Mirror: bmff.MirrorHorizontal}}, 3, 2, []byte{4, 5, 6, 1, 2, 3}},
		{[]bmff.Box{&bmff.ImageRotation{Angle: 1}, &bmff.ImageMirror{Mirror: 0}}, 2, 3, []byte{6, 3, 5, 2, 4, 1}},
		{[]bmff.Box{&bmff.ImageMirror{Mirror: 0}, &bmff.ImageRotation{Angle: 1}}, 2, 3, []byte{1, 4, 2, 5, 3, 6}},
	} {
		output, declared := orientHEIFProperties(orientationGrid(), test.properties)
		if declared != (len(test.properties) > 0) {
			t.Fatal("absent HEIF mirror property was treated as a reflection")
		}
		checkOrientationGrid(t, output, test.pixels, test.width, test.height)
	}
}

func orientationBox(kind string, payload []byte) []byte {
	box := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(box[:4], uint32(len(box)))
	copy(box[4:8], kind)
	copy(box[8:], payload)
	return box
}

func TestHEIFPrimaryItemContainerOrientation(t *testing.T) {
	// Synthetic BMFF metadata exercises the real primary-item parser, without
	// any camera files or dependency on an external codec fixture directory.
	ftyp := orientationBox("ftyp", []byte("heic\x00\x00\x00\x00mif1"))
	pitm := orientationBox("pitm", []byte{0, 0, 0, 0, 0, 1})
	info := append([]byte{2, 0, 0, 0, 0, 1, 0, 0}, []byte("hvc1\x00")...)
	iinf := orientationBox("iinf", append([]byte{0, 0, 0, 0, 0, 1}, orientationBox("infe", info)...))
	for _, declared := range []bool{false, true} {
		children := append(append([]byte{0, 0, 0, 0}, pitm...), iinf...)
		if declared {
			ipco := orientationBox("ipco", append(orientationBox("irot", []byte{3}), orientationBox("imir", []byte{0})...))
			// Primary item id 1, two properties, rotate then mirror.
			ipma := orientationBox("ipma", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 2, 1, 2})
			children = append(children, orientationBox("iprp", append(ipco, ipma...))...)
		}
		data := append(append([]byte(nil), ftyp...), orientationBox("meta", children)...)
		output, err := orientSourceImage(orientationGrid(), data, "heic")
		if err != nil {
			t.Fatal(err)
		}
		if declared {
			checkOrientationGrid(t, output, []byte{1, 4, 2, 5, 3, 6}, 2, 3)
		} else {
			checkOrientationGrid(t, output, []byte{1, 2, 3, 4, 5, 6}, 3, 2)
		}
	}
}

func pngChunk(kind string, payload []byte) []byte {
	chunk := make([]byte, len(payload)+12)
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], kind)
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	return chunk
}

func TestPNGOrientationPreservesAlpha(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, orientationGrid()); err != nil {
		t.Fatal(err)
	}
	data := append([]byte(nil), encoded.Bytes()[:33]...) // Signature + IHDR.
	data = append(data, pngChunk("eXIf", orientationTIFF(6, binary.BigEndian))...)
	data = append(data, encoded.Bytes()[33:]...)
	result, err := NormalizeInputImageFromBytes(nil, "image/png", data, "attachment")
	if err != nil || !result.PreservedAlpha || result.Width != 2 || result.Height != 3 {
		t.Fatalf("PNG EXIF normalization: %+v err=%v", result, err)
	}
	normalized, _ := base64.StdEncoding.DecodeString(result.Image.Data)
	img, _, err := image.Decode(bytes.NewReader(normalized))
	if err != nil {
		t.Fatal(err)
	}
	checkOrientationGrid(t, img, []byte{4, 1, 5, 2, 6, 3}, 2, 3)
}

func TestWebPEXIFAndMalformedOrientationMetadata(t *testing.T) {
	metadata := append([]byte("Exif\x00\x00"), orientationTIFF(8, binary.LittleEndian)...)
	webp := []byte("RIFF\x00\x00\x00\x00WEBP")
	chunk := make([]byte, 8)
	copy(chunk, "EXIF")
	binary.LittleEndian.PutUint32(chunk[4:], uint32(len(metadata)))
	webp = append(webp, chunk...)
	webp = append(webp, metadata...)
	binary.LittleEndian.PutUint32(webp[4:8], uint32(len(webp)-8))
	if sourceEXIFOrientation(webp, "webp") != 8 {
		t.Fatal("WebP EXIF not recognized")
	}
	valid := orientationTIFF(6, binary.LittleEndian)
	for i := 0; i < 22; i++ {
		if _, ok := tiffOrientation(valid[:i]); ok {
			t.Fatal("truncated TIFF orientation was accepted")
		}
	}
	for _, modify := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint32(b[4:8], 0xffffffff) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[8:10], 65535) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[14:18], 2) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[12:14], 4) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[18:20], 9) },
	} {
		data := append([]byte(nil), valid...)
		modify(data)
		if value, ok := tiffOrientation(data); ok || value != 1 {
			t.Fatal("invalid TIFF orientation was trusted")
		}
	}
	for _, data := range [][]byte{nil, {0xff, 0xd8, 0xff, 0xe1, 0xff, 0xff}, {0xff, 0xd8, 0xff}, []byte("RIFF\xff\xff\xff\xffWEBPEXIF\xff\xff\xff\xff")} {
		for _, format := range []string{"jpeg", "png", "webp"} {
			if sourceEXIFOrientation(data, format) != 1 {
				t.Fatal("malformed optional metadata changed orientation")
			}
		}
	}
}

func FuzzSourceImageOrientationMetadata(f *testing.F) {
	f.Add(orientationTIFF(6, binary.LittleEndian))
	f.Add([]byte("\xff\xd8\xff\xe1\xff\xff"))
	f.Add([]byte("\x89PNG\r\n\x1a\n"))
	f.Add([]byte("RIFF\x00\x00\x00\x00WEBP"))
	f.Fuzz(func(t *testing.T, data []byte) {
		value, _ := tiffOrientation(data)
		if value < 1 || value > 8 {
			t.Fatal("invalid orientation")
		}
		for _, format := range []string{"jpeg", "png", "webp"} {
			value := sourceEXIFOrientation(data, format)
			if value < 1 || value > 8 {
				t.Fatal("invalid container orientation")
			}
		}
	})
}
