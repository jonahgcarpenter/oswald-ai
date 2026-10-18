package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"

	"github.com/jdeng/goheif/heif"
	"github.com/jdeng/goheif/heif/bmff"
)

// orientSourceImage makes the declared display orientation part of the pixels
// before resizing/caching. Re-encoding strips that metadata, so retries and
// cached reads cannot rotate the normalized image a second time.
func orientSourceImage(img image.Image, data []byte, format string) (image.Image, error) {
	if format == "heic" || format == "heif" {
		file := heif.Open(bytes.NewReader(data))
		item, err := file.PrimaryItem()
		if err != nil {
			return nil, fmt.Errorf("read HEIF image orientation: %w", err)
		}
		transformed, declared := orientHEIFProperties(img, item.Properties)
		if declared {
			// Container transformations take precedence over EXIF: applying both
			// can rotate/mirror the same camera orientation twice.
			return transformed, nil
		}
		// HEIF display transforms are declared by the primary item's container
		// properties, not by potentially duplicative camera EXIF metadata.
		return img, nil
	}
	orientation := sourceEXIFOrientation(data, format)
	return applyImageOrientation(img, orientation), nil
}

func orientHEIFProperties(img image.Image, properties []bmff.Box) (image.Image, bool) {
	declared := false
	// HEIF transformative properties are applied in their association order.
	for _, property := range properties {
		switch property := property.(type) {
		case *bmff.ImageRotation:
			declared = true
			orientation := [...]int{1, 8, 3, 6}[property.Angle&3]
			img = applyImageOrientation(img, orientation)
		case *bmff.ImageMirror:
			declared = true
			orientation := 2 // Reflect about the vertical axis: reverse x.
			if property.Mirror == bmff.MirrorHorizontal {
				orientation = 4
			}
			img = applyImageOrientation(img, orientation)
		}
	}
	return img, declared
}

// sourceEXIFOrientation inspects only bounded container segments/chunks. Missing
// or malformed optional EXIF is ignored, never guessed from image contents.
func sourceEXIFOrientation(data []byte, format string) int {
	switch format {
	case "jpeg":
		if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
			break
		}
		for offset := 2; offset < len(data); {
			if data[offset] != 0xff {
				break
			}
			for offset < len(data) && data[offset] == 0xff {
				offset++
			}
			if offset >= len(data) {
				break
			}
			marker := data[offset]
			offset++
			if marker == 0xda || marker == 0xd9 {
				break // Do not scan compressed image bytes for metadata.
			}
			if marker == 0x01 || marker == 0xd8 || (marker >= 0xd0 && marker <= 0xd7) {
				continue
			}
			if len(data)-offset < 2 {
				break
			}
			length := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			if length < 2 || length > len(data)-offset {
				break
			}
			payload := data[offset+2 : offset+length]
			if marker == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
				if orientation, ok := tiffOrientation(payload); ok {
					return orientation
				}
			}
			offset += length
		}
	case "png":
		if len(data) < 8 || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
			break
		}
		for offset := 8; len(data)-offset >= 12; {
			length := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
			if length > uint64(len(data)-offset-12) {
				break
			}
			end := offset + 8 + int(length)
			if string(data[offset+4:offset+8]) == "eXIf" {
				if orientation, ok := tiffOrientation(data[offset+8 : end]); ok {
					return orientation
				}
			}
			if string(data[offset+4:offset+8]) == "IEND" {
				break
			}
			offset = end + 4
		}
	case "webp":
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			break
		}
		size := uint64(binary.LittleEndian.Uint32(data[4:8])) + 8
		if size < 12 || size > uint64(len(data)) {
			break
		}
		data = data[:int(size)]
		for offset := 12; len(data)-offset >= 8; {
			length := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
			if length > uint64(len(data)-offset-8) {
				break
			}
			end := offset + 8 + int(length)
			if string(data[offset:offset+4]) == "EXIF" {
				if orientation, ok := tiffOrientation(data[offset+8 : end]); ok {
					return orientation
				}
			}
			offset = end + int(length&1)
		}
	}
	return 1
}

// tiffOrientation reads only IFD0's single SHORT Orientation tag. All offsets
// and entry counts are bounded by the supplied metadata; other tags are ignored.
func tiffOrientation(data []byte) (int, bool) {
	data = bytes.TrimPrefix(data, []byte("Exif\x00\x00"))
	if len(data) < 8 {
		return 1, false
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1, false
	}
	if order.Uint16(data[2:4]) != 42 {
		return 1, false
	}
	offset := uint64(order.Uint32(data[4:8]))
	if offset < 8 || offset > uint64(len(data)-2) {
		return 1, false
	}
	entries := data[int(offset):]
	count := int(order.Uint16(entries[:2]))
	entries = entries[2:]
	if count > len(entries)/12 {
		return 1, false
	}
	for i := 0; i < count; i++ {
		entry := entries[i*12 : (i+1)*12]
		if order.Uint16(entry[:2]) != 0x0112 {
			continue
		}
		if order.Uint16(entry[2:4]) != 3 || order.Uint32(entry[4:8]) != 1 {
			return 1, false
		}
		orientation := int(order.Uint16(entry[8:10]))
		if orientation < 1 || orientation > 8 {
			return 1, false
		}
		return orientation, true
	}
	return 1, false
}

func applyImageOrientation(source image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return source
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	outWidth, outHeight := width, height
	if orientation >= 5 {
		outWidth, outHeight = height, width
	}
	output := image.NewNRGBA(image.Rect(0, 0, outWidth, outHeight))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			ox, oy := x, y
			switch orientation {
			case 2:
				ox = width - 1 - x
			case 3:
				ox, oy = width-1-x, height-1-y
			case 4:
				oy = height - 1 - y
			case 5:
				ox, oy = y, x
			case 6:
				ox, oy = height-1-y, x
			case 7:
				ox, oy = height-1-y, width-1-x
			case 8:
				ox, oy = y, width-1-x
			}
			output.Set(ox, oy, source.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return output
}
