package video_analyze

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"strings"
	"testing"
)

func writeTestFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0600)
}

func sampleJPEG(t *testing.T, width, height int, shade uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: shade, G: uint8(x % 256), B: uint8(y % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTimestampsForEvenSpacing(t *testing.T) {
	stamps := TimestampsFor(13.0, 4)
	want := []float64{1.625, 4.875, 8.125, 11.375}
	if len(stamps) != len(want) {
		t.Fatalf("stamps = %v", stamps)
	}
	for i := range want {
		if stamps[i] != want[i] {
			t.Fatalf("stamps = %v, want %v", stamps, want)
		}
	}
	if stamps[0] <= 0 || stamps[len(stamps)-1] >= 13.0 {
		t.Fatalf("endpoints not avoided: %v", stamps)
	}
	if len(TimestampsFor(9.0, 1)) != 1 || TimestampsFor(9.0, 1)[0] != 4.5 {
		t.Fatalf("single stamp = %v", TimestampsFor(9.0, 1))
	}
}

func TestRoundStamp(t *testing.T) {
	if got := RoundStamp(1.625); got != 1.6 {
		t.Fatalf("round = %v", got)
	}
	if got := RoundStamp(11.375); got != 11.4 {
		t.Fatalf("round = %v", got)
	}
}

func TestContactSheetGrid(t *testing.T) {
	frames := [][]byte{sampleJPEG(t, 64, 48, 10), sampleJPEG(t, 48, 64, 100), sampleJPEG(t, 32, 32, 200)}
	sheet, err := ContactSheet(frames)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(sheet))
	if err != nil {
		t.Fatalf("sheet is not a JPEG: %v", err)
	}
	if img.Bounds().Dx() != sheetCellEdge*2 || img.Bounds().Dy() != sheetCellEdge*2 {
		t.Fatalf("sheet bounds = %v", img.Bounds())
	}
	if _, err := ContactSheet(nil); err == nil {
		t.Fatal("empty sheet accepted")
	}
	six := append(append([][]byte{}, frames...), frames...)
	if _, err := ContactSheet(six); err == nil {
		// 6 frames must be split into 2 sheets by the caller, never tiled whole.
		t.Fatal("oversized sheet accepted")
	}
}

func TestFitCellCenters(t *testing.T) {
	cell := image.Rect(0, 0, 100, 100)
	wide := fitCell(cell, image.Rect(0, 0, 200, 100))
	if wide != image.Rect(0, 25, 100, 75) {
		t.Fatalf("wide fit = %v", wide)
	}
	tall := fitCell(cell, image.Rect(0, 0, 100, 200))
	if tall != image.Rect(25, 0, 75, 100) {
		t.Fatalf("tall fit = %v", tall)
	}
	if got := fitCell(cell, image.Rectangle{}); got != cell {
		t.Fatalf("empty fit = %v", got)
	}
}

func TestReadFrameFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	raw := sampleJPEG(t, 16, 12, 7)
	path := dir + "/frame-001.jpg"
	if err := writeTestFile(path, raw); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrameFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("round trip err=%v len=%d", err, len(got))
	}
	if _, err := ReadFrameFile(dir + "/missing.jpg"); err == nil {
		t.Fatal("missing frame accepted")
	}
}

func TestResultTextShapes(t *testing.T) {
	single := resultText("What?", []float64{7.0}, 13.0, 1, []string{"640×480"})
	if !strings.Contains(single, "1 still frame (t=7.0s of 13.0s") || strings.Contains(single, "Sheet") {
		t.Fatalf("single text = %q", single)
	}
	multi := resultText("What?", []float64{1.6, 4.9}, 13.0, 1, []string{"640×480", "640×480"})
	if !strings.Contains(multi, "2 still frames") || !strings.Contains(multi, "Frame 2/2 @ 4.9s") {
		t.Fatalf("multi text = %q", multi)
	}
	sheet := resultText("What?", []float64{1.0, 2.0, 3.0, 4.0, 5.0}, 20.0, 2, nil)
	if !strings.Contains(sheet, "Sheet 1/2: top-left t=1.0s") || !strings.Contains(sheet, "Sheet 2/2: top-left t=5.0s") {
		t.Fatalf("sheet text = %q", sheet)
	}
	for _, text := range []string{single, multi, sheet} {
		if !strings.Contains(text, "Question: What?") || !strings.Contains(text, "not perceived") {
			t.Fatalf("missing caveat/question: %q", text)
		}
	}
}
