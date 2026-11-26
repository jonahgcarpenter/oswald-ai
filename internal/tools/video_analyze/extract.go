package video_analyze

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	xdraw "golang.org/x/image/draw"
)

// Extraction bounds. The input cap matches the output attachment cap so one
// video never exceeds what a single result could carry; the duration cap keeps
// sampling meaningful (longer videos should be trimmed first).
const (
	maxInputBytes      = 25 << 20
	maxDurationSeconds = 600.0
	extractTimeout     = 120 * time.Second
	sheetCellEdge      = 960
)

// TimestampsFor returns n evenly spaced sample times across duration,
// avoiding the exact endpoints where players show black frames.
func TimestampsFor(duration float64, n int) []float64 {
	stamps := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		stamps = append(stamps, duration*(float64(i)+0.5)/float64(n))
	}
	return stamps
}

// RoundStamp rounds a timestamp to the 0.1s labels used in results.
func RoundStamp(value float64) float64 {
	return float64(int(value*10+0.5)) / 10
}

// ExtractFrames pulls one JPEG per stamp from src with ffmpeg. Evenly spaced
// requests run a single fps-filter pass; explicit timestamps seek
// individually. Outputs are 0600 files under dir, ordered like stamps.
// duration is the probed video length, used to scale the fps filter.
func ExtractFrames(ctx context.Context, ffmpeg, src string, stamps []float64, duration float64, dir string) ([]string, error) {
	binary := strings.TrimSpace(ffmpeg)
	if binary == "" {
		binary = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()
	if even(stamps) {
		return extractEven(ctx, binary, src, len(stamps), duration, dir)
	}
	return extractStamps(ctx, binary, src, stamps, dir)
}

// even reports whether stamps look evenly spaced (the frames-count path).
func even(stamps []float64) bool {
	if len(stamps) < 2 {
		return true
	}
	gap := stamps[1] - stamps[0]
	for i := 2; i < len(stamps); i++ {
		if diff := stamps[i] - stamps[i-1]; diff < gap*0.9 || diff > gap*1.1 {
			return false
		}
	}
	return true
}

func extractEven(ctx context.Context, ffmpeg, src string, n int, duration float64, dir string) ([]string, error) {
	rate := float64(n)
	if duration > 0 {
		rate = float64(n) / duration
	}
	pattern := filepath.Join(dir, "frame-%03d.jpg")
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-i", src, "-vf", fmt.Sprintf("fps=%.6f", rate), "-q:v", "3", pattern)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("frame extraction failed: %s", firstLine(out))
	}
	entries, err := filepath.Glob(filepath.Join(dir, "frame-*.jpg"))
	if err != nil || len(entries) == 0 {
		return nil, fmt.Errorf("frame extraction produced no frames")
	}
	ordered := entries
	if len(ordered) > n {
		ordered = ordered[:n]
	}
	if len(ordered) < n {
		return nil, fmt.Errorf("frame extraction produced fewer frames than requested")
	}
	for _, path := range ordered {
		if err := chmodPrivate(path); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func extractStamps(ctx context.Context, ffmpeg, src string, stamps []float64, dir string) ([]string, error) {
	out := make([]string, 0, len(stamps))
	for i, stamp := range stamps {
		path := filepath.Join(dir, fmt.Sprintf("frame-%03d.jpg", i+1))
		cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-ss", fmt.Sprintf("%.1f", stamp), "-i", src, "-frames:v", "1", "-q:v", "3", path)
		if combined, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("frame extraction failed: %s", firstLine(combined))
		}
		if err := chmodPrivate(path); err != nil {
			return nil, err
		}
		out = append(out, path)
	}
	return out, nil
}

func chmodPrivate(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("frame extraction produced no frames")
	}
	return os.Chmod(path, 0600)
}

func firstLine(out []byte) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "ffmpeg reported no details"
	}
	if i := strings.Index(text, "\n"); i >= 0 {
		text = text[:i]
	}
	const maxLen = 200
	if len(text) > maxLen {
		text = text[:maxLen]
	}
	return text
}

// ContactSheet tiles up to 4 JPEG frames into one labeled-order 2x2 JPEG:
// left-to-right, top-to-bottom matches frame order. The result text maps
// tile positions to timestamps.
func ContactSheet(frames [][]byte) ([]byte, error) {
	if len(frames) == 0 || len(frames) > 4 {
		return nil, fmt.Errorf("contact sheet needs 1 to 4 frames")
	}
	decoded := make([]image.Image, 0, len(frames))
	for _, raw := range frames {
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("contact sheet decode failed")
		}
		decoded = append(decoded, img)
	}
	sheet := image.NewRGBA(image.Rect(0, 0, sheetCellEdge*2, sheetCellEdge*2))
	draw.Draw(sheet, sheet.Bounds(), image.Black, image.Point{}, draw.Src)
	for i, img := range decoded {
		cell := image.Rect((i%2)*sheetCellEdge, (i/2)*sheetCellEdge, (i%2+1)*sheetCellEdge, (i/2+1)*sheetCellEdge)
		xdraw.CatmullRom.Scale(sheet, fitCell(cell, img.Bounds()), img, img.Bounds(), draw.Over, nil)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, sheet, &jpeg.Options{Quality: 80}); err != nil {
		return nil, fmt.Errorf("contact sheet encode failed")
	}
	return buf.Bytes(), nil
}

// fitCell returns the largest aspect-preserving rectangle of src centered in
// cell, letterboxing the remainder.
func fitCell(cell, src image.Rectangle) image.Rectangle {
	sw, sh := src.Dx(), src.Dy()
	if sw <= 0 || sh <= 0 {
		return cell
	}
	cw, ch := cell.Dx(), cell.Dy()
	scaleW := float64(cw) / float64(sw)
	scaleH := float64(ch) / float64(sh)
	scale := scaleW
	if scaleH < scaleW {
		scale = scaleH
	}
	dw, dh := int(float64(sw)*scale+0.5), int(float64(sh)*scale+0.5)
	ox := cell.Min.X + (cw-dw)/2
	oy := cell.Min.Y + (ch-dh)/2
	return image.Rect(ox, oy, ox+dw, oy+dh)
}

// ReadFrameFile reads one extracted frame back with the same privacy and size
// discipline as workspace image reads.
func ReadFrameFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("frame is not a regular file")
	}
	if info.Size() > maxInputBytes {
		return nil, fmt.Errorf("frame exceeds the size limit")
	}
	data := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, readErr := f.Read(chunk)
		if n > 0 {
			if int64(len(data)+n) > maxInputBytes+1 {
				return nil, fmt.Errorf("frame exceeds the size limit")
			}
			data = append(data, chunk[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("frame is empty")
	}
	return data, nil
}
