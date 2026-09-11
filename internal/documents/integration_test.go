//go:build document_integration

package documents

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Generate test documents entirely from synthetic content, not real transcripts.
func syntheticPDF(t *testing.T, scanned bool) []byte {
	t.Helper()
	stream := "BT /F1 16 Tf 30 120 Td (Synthetic document extraction verifies native text without any private content.) Tj ET"
	resources := "/Font << /F1 5 0 R >>"
	object5 := []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	if scanned {
		base := image.NewGray(image.Rect(0, 0, 600, 100))
		draw.Draw(base, base.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		d := font.Drawer{Dst: base, Src: image.NewUniform(color.Black), Face: basicfont.Face7x13, Dot: fixed.P(15, 40)}
		d.DrawString("SYNTHETIC DOCUMENT EXTRACTION TEST")
		large := image.NewGray(image.Rect(0, 0, 1800, 300))
		draw.NearestNeighbor.Scale(large, large.Bounds(), base, base.Bounds(), draw.Src, nil)
		var compressed bytes.Buffer
		z := zlib.NewWriter(&compressed)
		if _, err := z.Write(large.Pix); err != nil {
			t.Fatal(err)
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		object5 = []byte(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 1800 /Height 300 /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /FlateDecode /Length %d >>\nstream\n", compressed.Len()))
		object5 = append(object5, compressed.Bytes()...)
		object5 = append(object5, []byte("\nendstream")...)
		resources = "/XObject << /Im0 5 0 R >>"
		stream = "q 600 0 0 100 0 50 cm /Im0 Do Q"
	}
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 800 200] /Resources << " + resources + " >> /Contents 4 0 R >>"),
		[]byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)), object5,
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, pdf.Len())
		fmt.Fprintf(&pdf, "%d 0 obj\n", i+1)
		pdf.Write(obj)
		pdf.WriteString("\nendobj\n")
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return pdf.Bytes()
}

func TestIntegrationPDFNative(t *testing.T) {
	if !ProbeCapabilities().PDF {
		t.Skip("fixed-path Poppler/prlimit unavailable")
	}
	r, err := NewExtractor(nil).Extract(context.Background(), "synthetic.pdf", "", syntheticPDF(t, false))
	if err != nil || len(r.Chunks) != 1 || r.Chunks[0].Method != "native" || !strings.Contains(r.Chunks[0].Text, "Synthetic document extraction") {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestIntegrationPDFOCR(t *testing.T) {
	if !ProbeCapabilities().OCR {
		t.Skip("fixed-path Poppler/Tesseract English/prlimit unavailable")
	}
	r, err := NewExtractor(nil).Extract(context.Background(), "scan.pdf", "", syntheticPDF(t, true))
	if err != nil || len(r.Chunks) != 1 || r.Chunks[0].Method != "ocr" || !strings.Contains(strings.ToUpper(r.Chunks[0].Text), "EXTRACTION TEST") {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestIntegrationOffice(t *testing.T) {
	if !ProbeCapabilities().Office {
		t.Skip("fixed-path LibreOffice/Poppler/prlimit unavailable")
	}
	r, err := NewExtractor(nil).Extract(context.Background(), "synthetic.rtf", "", []byte(`{\rtf1\ansi Synthetic document extraction verifies office conversion without any private content.}`))
	if err != nil || len(r.Chunks) != 1 || r.Chunks[0].Method != "libreoffice-native" || !strings.Contains(r.Chunks[0].Text, "Synthetic document extraction") {
		t.Fatalf("%+v %v", r, err)
	}
}
