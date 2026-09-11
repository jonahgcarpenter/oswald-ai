package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func archiveFixture(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, content := range files {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestNativeTextFormats(t *testing.T) {
	for _, tc := range []struct{ ext, input, want string }{
		{".txt", "hello", "hello"}, {".go", "package main", "package main"}, {".md", "# Header", "# Header"},
		{".json", `{"a":1}`, `{"a":1}`}, {".xml", `<root><a>hello &amp; world</a></root>`, "hello & world"},
		{".html", `<style>SECRET</style><p>Hello &amp; world</p><script>SECRET</script>`, "Hello & world"},
		{".html", `<template><script>SECRET</script>SECRET</template><p>visible</p>`, "visible"},
		{".xml", "\xef\xbb\xbf<root>bom</root>", "bom"},
		{".py", "  indented\n", "  indented\n"},
		{".csv", "\"a,b\",c\n", "a,b\tc"}, {".tsv", "a\tb\n", "a\tb"},
	} {
		t.Run(tc.ext, func(t *testing.T) {
			e := NewExtractor(nil)
			e.run = func(context.Context, string, string, ...string) ([]byte, error) {
				t.Fatal("native extraction invoked a tool")
				return nil, nil
			}
			r, err := e.Extract(context.Background(), "document"+tc.ext, "", []byte(tc.input))
			if err != nil || len(r.Chunks) != 1 || r.Chunks[0].Text != tc.want || r.Chunks[0].Ordinal != 1 || r.Partial {
				t.Fatalf("result=%+v err=%v", r, err)
			}
		})
	}
	r, err := NewExtractor(nil).Extract(context.Background(), "", "text/plain; charset=utf-8", []byte("MIME fallback"))
	if err != nil || r.Chunks[0].Text != "MIME fallback" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestXLSXEmptyAndMissingFormulaCaches(t *testing.T) {
	data := xlsxFixture(t, `<c t="str"><f>IF(1,"","")</f><v></v></c><c t="b"><f>1=1</f></c>`)
	r, err := NewExtractor(nil).Extract(context.Background(), "formula.xlsx", "", data)
	if err != nil || len(r.Chunks) != 2 || !r.Partial || r.Chunks[0].Text != " [cached formula value]" || r.Chunks[1].Text != "[formula: no cached value]" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestLimitsAndInvalidText(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"large.txt", make([]byte, maxInput+1), ErrLimit}, {"binary.txt", []byte{0xff, 0}, ErrInvalid},
		{"file.xls", []byte{0xd0, 0xcf, 0x11, 0xe0}, ErrUnsupported}, {"file.exe", []byte("abc"), ErrUnsupported},
		{"bad.json", []byte(`{"a":`), ErrInvalid}, {"bad.csv", []byte("\"unclosed"), ErrInvalid},
		{"entity.xml", []byte(`<!DOCTYPE a [<!ENTITY x SYSTEM "file:///etc/passwd">]><a>&x;</a>`), ErrInvalid},
		{"roots.xml", []byte(`<a/><b/>`), ErrInvalid}, {"empty.xml", nil, ErrInvalid},
		{"depth.xml", []byte(strings.Repeat("<a>", 65) + strings.Repeat("</a>", 65)), ErrLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewExtractor(nil).Extract(context.Background(), tc.name, "", tc.data)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v got %v", tc.want, err)
			}
		})
	}
	r, err := NewExtractor(nil).Extract(context.Background(), "unicode.txt", "", []byte(strings.Repeat("\u20ac", maxText/3+2)))
	if err != nil || !r.Partial || len(r.Chunks) != 1 || len(r.Chunks[0].Text) > maxText || !utf8.ValidString(r.Chunks[0].Text) {
		t.Fatalf("invalid truncation: %v", err)
	}
}

func xlsxFixture(t *testing.T, cells string) []byte {
	return archiveFixture(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="urn:rels"><sheets><sheet name="Synthetic" r:id="r1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="r1" Target="worksheets/sheet7.xml" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst><si><r><t>Hello </t></r><r><t>world</t></r><rPh><t>not visible</t></rPh></si></sst>`,
		"xl/worksheets/sheet7.xml":   `<worksheet><sheetData><row r="3">` + cells + `</row></sheetData></worksheet>`,
	})
}

func TestXLSXCells(t *testing.T) {
	data := xlsxFixture(t, `<c r="A3" t="s"><v>0</v></c><c r="C3" t="inlineStr"><is><r><t>inline </t></r><r><t>rich</t></r></is></c><c t="b"><v>1</v></c><c><f>1+1</f><v>2</v></c><c><f>WEBSERVICE("https://invalid.example")</f></c><c t="e"><v>#DIV/0!</v></c><c t="d"><v>2026-09-11</v></c><c s="1"><v>45000</v></c>`)
	r, err := NewExtractor(nil).Extract(context.Background(), "test.xlsx", "", data)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range r.Chunks {
		got = append(got, c.Text)
	}
	want := []string{"Hello world", "inline rich", "true", "2 [cached formula value]", "[formula: no cached value]", "#DIV/0!", "2026-09-11", "45000"}
	if !reflect.DeepEqual(got, want) || !r.Partial || r.Chunks[1].Locator != "sheet:1 row:3 column:3" || r.Chunks[2].Locator != "sheet:1 row:3 column:4" {
		t.Fatalf("%+v", r)
	}
	for _, cells := range []string{`<c t="s"><v>99</v></c>`, `<c r="XFE3"><v>1</v></c>`, `<c r="A4"><v>1</v></c>`, `<c t="b"><v>9</v></c>`} {
		_, err := NewExtractor(nil).Extract(context.Background(), "bad.xlsx", "", xlsxFixture(t, cells))
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected invalid, got %v", err)
		}
	}
}

func TestODSCellsAndRepetition(t *testing.T) {
	data := archiveFixture(t, map[string]string{"content.xml": `<document xmlns:table="urn:table" xmlns:office="urn:office" xmlns:text="urn:text"><table:table><table:table-row table:number-rows-repeated="2"><table:table-cell table:number-columns-repeated="2" office:value-type="string"><text:p>A<text:s text:c="2"/><text:span>B</text:span></text:p><text:p>C</text:p></table:table-cell><table:covered-table-cell/><table:table-cell office:value-type="boolean" office:boolean-value="true"/><table:table-cell office:value-type="float" office:value="2" table:formula="of:=1+1"/></table:table-row><table:table-row table:number-rows-repeated="999999"><table:table-cell/></table:table-row><table:table-row><table:table-cell office:value-type="date" office:date-value="2026-09-11"/></table:table-row></table:table></document>`})
	r, err := NewExtractor(nil).Extract(context.Background(), "test.ods", "", data)
	if err != nil || len(r.Chunks) != 9 || r.Partial {
		t.Fatalf("%+v %v", r, err)
	}
	if r.Chunks[0].Text != "A  B\nC" || r.Chunks[2].Text != "true" || r.Chunks[2].Locator != "sheet:1 row:1 column:4" || r.Chunks[8].Locator != "sheet:1 row:1000002 column:1" {
		t.Fatalf("%+v", r)
	}
	data = archiveFixture(t, map[string]string{"content.xml": `<document><table><table-row number-rows-repeated="10001"><table-cell value-type="string" string-value="x"/></table-row></table></document>`})
	r, err = NewExtractor(nil).Extract(context.Background(), "repeat.ods", "", data)
	if err != nil || !r.Partial || len(r.Chunks) != maxCells {
		t.Fatalf("chunks=%d partial=%v err=%v", len(r.Chunks), r.Partial, err)
	}
}

func TestArchiveGuards(t *testing.T) {
	for _, name := range []string{"../escape.xml", "/absolute.xml", "a/../b.xml", "a\\b.xml", "a:b.xml"} {
		_, err := openArchive(context.Background(), archiveFixture(t, map[string]string{name: "<a/>"}))
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, mode := range []string{"duplicate", "symlink", "ratio", "count", "size", "crc"} {
		t.Run(mode, func(t *testing.T) {
			var b bytes.Buffer
			w := zip.NewWriter(&b)
			count := 1
			if mode == "duplicate" {
				count = 2
			}
			if mode == "count" {
				count = 2049
			}
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("file%d.xml", i)
				if mode == "duplicate" {
					name = "same.xml"
				}
				h := &zip.FileHeader{Name: name, Method: zip.Store}
				if mode == "symlink" {
					h.SetMode(os.ModeSymlink | 0700)
				}
				if mode == "ratio" || mode == "size" {
					h.Method = zip.Deflate
				}
				f, err := w.CreateHeader(h)
				if err != nil {
					t.Fatal(err)
				}
				body := "<a>unique-crc-canary</a>"
				if mode == "ratio" {
					body = strings.Repeat("a", 2<<20)
				}
				if mode == "size" {
					body = strings.Repeat("a", maxArchive+1)
				}
				if _, err = f.Write([]byte(body)); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			data := b.Bytes()
			if mode == "crc" {
				index := bytes.Index(data, []byte("unique-crc-canary"))
				data[index] = 'X'
			}
			if _, err := openArchive(context.Background(), data); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func writePageFixture(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, image.NewGray(image.Rect(0, 0, 10, 10))); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPDFSequentialPageAndOCRLimits(t *testing.T) {
	e := NewExtractor(nil)
	native, ocr, render := 0, 0, 0
	var temp string
	e.run = func(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
		temp = dir
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 3*time.Minute {
			t.Fatal("missing deadline")
		}
		switch program {
		case pdfinfo:
			return []byte("Pages: 102\n"), nil
		case pdftotext:
			native++
			if args[1] != fmt.Sprint(native) || args[3] != fmt.Sprint(native) {
				t.Fatal("page selection")
			}
			return []byte("short"), nil
		case pdftoppm:
			render++
			if render != ocr+1 || !strings.Contains(strings.Join(args, " "), "-scale-to 3000") {
				t.Fatal("render bound/order")
			}
			writePageFixture(t, args[len(args)-1]+".png")
			return nil, nil
		case tesseract:
			ocr++
			if args[2] != "-l" || args[3] != "eng" {
				t.Fatal("OCR language")
			}
			return []byte("recognized page"), nil
		default:
			t.Fatalf("unexpected program %s", program)
			return nil, nil
		}
	}
	r, err := e.Extract(context.Background(), "../../--secret.pdf", "", []byte("synthetic PDF"))
	if err != nil || !r.Partial || len(r.Chunks) != 100 || native != 100 || ocr != 25 || render != 25 || r.Chunks[24].Method != "ocr" || r.Chunks[25].Method != "native" {
		t.Fatalf("chunks=%d native=%d ocr=%d err=%v", len(r.Chunks), native, ocr, err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("temp directory retained")
	}
}

func TestOfficeProfileAndFailureCleanup(t *testing.T) {
	var dirs []string
	for _, fail := range []bool{false, true} {
		e := NewExtractor(nil)
		e.run = func(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
			switch program {
			case libreoffice:
				dirs = append(dirs, dir)
				profile, err := os.ReadFile(filepath.Join(dir, "profile/user/registrymodifications.xcu"))
				if err != nil {
					t.Fatal(err)
				}
				if string(profile) != officeProfile || !strings.Contains(string(profile), "BlockUntrustedRefererLinks") || !strings.Contains(strings.Join(args, " "), "--infilter=Rich Text Format") || args[len(args)-1] != filepath.Join(dir, "input.rtf") {
					t.Fatal("unsafe conversion configuration")
				}
				if fail {
					return nil, errors.New("private converter failure")
				}
				return nil, os.WriteFile(filepath.Join(dir, "input.pdf"), []byte("fake"), 0600)
			case pdfinfo:
				return []byte("Pages: 1"), nil
			case pdftotext:
				return []byte(strings.Repeat("Native synthetic document text. ", 3)), nil
			default:
				t.Fatal("unexpected OCR")
				return nil, nil
			}
		}
		r, err := e.Extract(context.Background(), "--private;secret.rtf", "", []byte(`{\rtf1 synthetic}`))
		if (err != nil) != fail {
			t.Fatalf("%+v %v", r, err)
		}
		if !fail && r.Chunks[0].Method != "libreoffice-native" {
			t.Fatal(r)
		}
	}
	if dirs[0] == dirs[1] {
		t.Fatal("profile reused")
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("temp files retained")
		}
	}
}

func TestPermitCancellation(t *testing.T) {
	e := NewExtractor(nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	e.run = func(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
		close(entered)
		<-release
		return nil, ErrInvalid
	}
	go func() { _, err := e.Extract(context.Background(), "a.pdf", "", nil); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.Extract(ctx, "b.txt", "", []byte("test"))
	close(release)
	<-done
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r, err := e.Extract(context.Background(), "c.txt", "", []byte("permit released"))
	if err != nil || len(r.Chunks) != 1 {
		t.Fatal(err)
	}
}

func TestSafeSingleMeasurement(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		var logs bytes.Buffer
		log := config.NewLogger(level)
		log.SetOutput(&logs)
		e := NewExtractor(log)
		e.run = func(context.Context, string, string, ...string) ([]byte, error) {
			return nil, errors.New("PRIVATE_ERROR_CANARY")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for _, tc := range []struct {
			ctx        context.Context
			file, text string
		}{
			{context.Background(), "PRIVATE_FILENAME.txt", "PRIVATE_TEXT"}, {context.Background(), "file.exe", "x"},
			{context.Background(), "file.pdf", "PRIVATE_PDF"}, {ctx, "file.txt", "x"}, {context.Background(), "empty.txt", ""},
			{context.Background(), "large.txt", strings.Repeat("x", maxText+1)},
		} {
			_, _ = e.Extract(tc.ctx, tc.file, "", []byte(tc.text))
		}
		if strings.Contains(logs.String(), "PRIVATE_") {
			t.Fatal("private data logged")
		}
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		if len(lines) != 6 {
			t.Fatalf("%d measurements", len(lines))
		}
		for _, line := range lines {
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatal(err)
			}
			if record["event"] != "documents.extraction.complete" || record["level"] != "info" || record["record_kind"] != "measurement" {
				t.Fatal(record)
			}
			if _, ok := record["duration_ms"].(float64); !ok {
				t.Fatal("duration not numeric")
			}
			if _, ok := record["is_partial"].(bool); !ok {
				t.Fatal("partial not boolean")
			}
		}
	}
}

func TestOutputAndDiskBounds(t *testing.T) {
	var b boundedOutput
	if _, err := b.Write(make([]byte, maxText)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("x")); !errors.Is(err, ErrLimit) || !b.exceeded || b.Len() != maxText {
		t.Fatal("stdout unbounded")
	}
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(maxFile + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if !errors.Is(checkDisk(dir), ErrLimit) {
		t.Fatal("large file accepted")
	}
	if err = os.Remove(filepath.Join(dir, "large")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(checkDisk(dir), ErrInvalid) {
		t.Fatal("symlink accepted")
	}
}

func TestProcessConfigurationDoesNotInheritEnvironment(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "PRIVATE_ENV_CANARY")
	t.Setenv("HTTP_PROXY", "http://PRIVATE_PROXY_CANARY")
	t.Setenv("LD_PRELOAD", "PRIVATE_PRELOAD_CANARY")
	cmd := processCommand(context.Background(), "/tmp/synthetic-private-dir", pdftotext, "input.pdf", "-")
	if cmd.Path != prlimit || cmd.Dir != "/tmp/synthetic-private-dir" || !cmd.SysProcAttr.Setpgid || cmd.Cancel == nil || cmd.WaitDelay != time.Second {
		t.Fatal("unsafe process configuration")
	}
	if strings.Contains(strings.Join(cmd.Env, " "), "PRIVATE_") {
		t.Fatal("environment inherited")
	}
	for _, limit := range []string{"--fsize=33554432:33554432", "--as=1073741824:1073741824", "--cpu=150:150", "--nofile=128:128", "--core=0:0"} {
		if !strings.Contains(strings.Join(cmd.Args, " "), limit) {
			t.Fatalf("missing %s", limit)
		}
	}
}

func TestActiveCancellationAndPartialFailure(t *testing.T) {
	for _, cancelWork := range []bool{true, false} {
		e := NewExtractor(nil)
		ctx, cancel := context.WithCancel(context.Background())
		var temp string
		calls := 0
		e.run = func(ctx context.Context, dir, program string, args ...string) ([]byte, error) {
			temp = dir
			if program == pdfinfo {
				return []byte("Pages: 2"), nil
			}
			calls++
			if calls == 1 {
				return []byte(strings.Repeat("Synthetic native text. ", 4)), nil
			}
			if cancelWork {
				cancel()
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return nil, ErrLimit
		}
		r, err := e.Extract(ctx, "test.pdf", "", nil)
		cancel()
		want := ErrLimit
		if cancelWork {
			want = context.Canceled
		}
		if !errors.Is(err, want) || !r.Partial || len(r.Chunks) != 1 {
			t.Fatalf("%+v %v", r, err)
		}
		if _, err := os.Stat(temp); !os.IsNotExist(err) {
			t.Fatal("temp directory survived cancellation/failure")
		}
	}
}

func TestODSExpandedRowBound(t *testing.T) {
	cell := `<table-cell><p>A` + strings.Repeat(`<s c="10000"/>`, 60) + `</p></table-cell>`
	data := archiveFixture(t, map[string]string{"content.xml": `<document><table><table-row>` + cell + cell + `</table-row></table></document>`})
	_, err := NewExtractor(nil).Extract(context.Background(), "expanded.ods", "", data)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("expanded row accepted: %v", err)
	}
}

func TestPagePixelLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "page.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, image.NewGray(image.Rect(0, 0, 3001, 1))); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if !errors.Is(validatePageImage(path), ErrLimit) {
		t.Fatal("oversized raster accepted")
	}
}
