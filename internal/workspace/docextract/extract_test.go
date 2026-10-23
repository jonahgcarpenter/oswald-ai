package docextract

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractText(t *testing.T) {
	text, err := Extract("notes.txt", []byte("hello\nworld\n"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "hello") {
		t.Fatalf("missing content: %q", text)
	}
	if _, err := Extract("notes.txt", []byte{0xff, 0xfe, 0x00}); err == nil {
		t.Fatal("invalid utf8 should fail")
	}
}

func TestExtractDOCX(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte(`<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Hello </w:t></w:r><w:r><w:t>docx</w:t></w:r></w:p></w:body></w:document>`))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	text, err := Extract("report.docx", buf.Bytes())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "Hello") || !strings.Contains(text, "docx") {
		t.Fatalf("missing content: %q", text)
	}
	if _, err := Extract("report.docx", []byte("not a zip")); err == nil {
		t.Fatal("malformed docx should fail")
	}
}

func TestExtractXLSX(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	write := func(name, body string) {
		t.Helper()
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(body))
	}
	write("xl/workbook.xml", `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheets><sheet name="Data" sheetId="1"/></sheets></workbook>`)
	write("xl/sharedStrings.xml", `<?xml version="1.0"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>Name</t></si></sst>`)
	write("xl/worksheets/sheet1.xml", `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row><c r="A1" t="s"><v>0</v></c><c r="B1"><v>42</v></c></row></sheetData></worksheet>`)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	text, err := Extract("data.xlsx", buf.Bytes())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "Name") || !strings.Contains(text, "42") {
		t.Fatalf("missing content: %q", text)
	}
}

func TestExtractPDFFallbackWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	// Minimal single-page PDF with an unfiltered text-showing stream.
	raw := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n" +
		"2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
		"3 0 obj<</Type/Page/Parent 2 0 R/Contents 4 0 R>>endobj\n" +
		"4 0 obj<</Length 44>>stream\nBT (Hello pdf text) Tj ET\nendstream\nendobj\ntrailer<</Root 1 0 R>>")
	text, err := Extract("doc.pdf", raw)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "Hello pdf text") {
		t.Fatalf("missing content: %q", text)
	}
	if _, err := Extract("doc.pdf", []byte("%PDF-1.4 empty")); err == nil {
		t.Fatal("textless pdf should fail")
	}
}

func TestExtractPDFPrefersPdftotext(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "pdftotext")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf 'rendered by tool\\n'"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	raw := []byte("%PDF-1.4\ntrailer<</Root 1 0 R>>")
	text, err := Extract("doc.pdf", raw)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "rendered by tool") {
		t.Fatalf("expected tool output, got %q", text)
	}
}

func TestExtractPDFDecodesUTF16Hex(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	// "Hello" as UTF-16BE with BOM inside an unfiltered content stream.
	raw := []byte("%PDF-1.4\n4 0 obj<</Length 60>>stream\nBT <FEFF00480065006C006C006F> Tj ET\nendstream\nendobj\ntrailer<</Root 1 0 R>>")
	text, err := Extract("doc.pdf", raw)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "Hello") {
		t.Fatalf("missing content: %q", text)
	}
}

func TestExtractPDFSkipsUndecodableFilteredStream(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	// Declared FlateDecode but the bytes are not deflate: must fail closed,
	// never surface the compressed bytes as text.
	raw := []byte("%PDF-1.4\n1 0 obj<</Length 40 /Filter /FlateDecode>>stream\n(not deflate at all (abc) $*#%)\nendstream\nendobj\ntrailer<</Root 1 0 R>>")
	if _, err := Extract("doc.pdf", raw); err == nil {
		t.Fatal("undecodable filtered stream should fail, not return garbage")
	}
}

func TestLooksGarbled(t *testing.T) {
	garbled := "1|(*) ) # - + ) / - 0 + * $ 3_ * ( + - # % $ 2|/ * $ - # + ) * $ / 3|/ * # 0 w"
	if !looksGarbled(garbled) {
		t.Fatalf("symbol soup should read as garbled: %q", garbled)
	}
	clean := "Jonah Carpenter\nSoftware engineer with ten years of experience building distributed systems\nGo Python Linux SQL Docker Kubernetes AWS Terraform Postgres Redis"
	if looksGarbled(clean) {
		t.Fatalf("readable resume text should not read as garbled: %q", clean)
	}
}

func TestIsExtractable(t *testing.T) {
	if !IsExtractable("a.PDF") || !IsExtractable("b.docx") {
		t.Fatal("pdf/docx should be extractable")
	}
	if IsExtractable("a.png") || IsExtractable("a.exe") {
		t.Fatal("image/exe should not be extractable")
	}
}
