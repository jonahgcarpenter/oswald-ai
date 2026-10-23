// Package docextract converts inbound documents to readable text.
package docextract

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/ascii85"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxDocumentBytes = 10 << 20
	maxXLSXRows      = 5000
	maxXLSXCols      = 256
)

// ExtractError marks a supported-looking document that cannot render as text.
type ExtractError struct{ msg string }

func (e *ExtractError) Error() string { return e.msg }

// IsExtractable reports whether name has an extraction-supported extension.
func IsExtractable(name string) bool {
	switch strings.ToLower(strings.TrimSpace(filepath.Ext(name))) {
	case ".txt", ".md", ".csv", ".log", ".json", ".docx", ".xlsx", ".pdf":
		return true
	}
	return false
}

// Extract renders filename/data to text. Text formats pass through; office
// formats decode with stdlib zip/xml; PDFs use a best-effort stream scan.
func Extract(filename string, data []byte) (string, error) {
	if len(data) == 0 || len(data) > maxDocumentBytes {
		return "", &ExtractError{"document size out of bounds"}
	}
	switch strings.ToLower(strings.TrimSpace(filepath.Ext(filename))) {
	case ".txt", ".md", ".csv", ".log", ".json":
		return extractText(data)
	case ".docx":
		return extractDOCX(data)
	case ".xlsx":
		return extractXLSX(data)
	case ".pdf":
		return extractPDF(data, filename)
	}
	return "", &ExtractError{fmt.Sprintf("unsupported document type: %s", filepath.Ext(filename))}
}

func extractText(data []byte) (string, error) {
	if !utf8.Valid(data) {
		return "", &ExtractError{"document is not valid text"}
	}
	for _, b := range data {
		if b == 0 {
			return "", &ExtractError{"document is binary"}
		}
	}
	text := strings.TrimRight(string(data), "\n")
	if strings.TrimSpace(text) == "" {
		return "", &ExtractError{"document contains no extractable text"}
	}
	return text + "\n", nil
}

// --- DOCX ---

type docxBody struct {
	Paragraphs []docxPara `xml:"p"`
}

type docxPara struct {
	Runs []docxRun `xml:"r"`
}

type docxRun struct {
	Text  []string `xml:"t"`
	Tabs  []string `xml:"tab"`
	Break []string `xml:"br"`
	Cr    []string `xml:"cr"`
}

func extractDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", &ExtractError{fmt.Sprintf("not a valid document: %v", err)}
	}
	var doc []byte
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				return "", &ExtractError{err.Error()}
			}
			doc, err = io.ReadAll(io.LimitReader(rc, maxDocumentBytes))
			rc.Close()
			if err != nil {
				return "", &ExtractError{err.Error()}
			}
			break
		}
	}
	if doc == nil {
		return "", &ExtractError{"not a valid document: missing word/document.xml"}
	}
	var body struct {
		XMLName xml.Name `xml:"document"`
		Body    struct {
			Paragraphs []docxPara `xml:"p"`
		} `xml:"body"`
	}
	// Namespace-agnostic decode: strip prefixes by decoding generically.
	paras, err := docxParagraphs(doc)
	if err != nil {
		return "", err
	}
	_ = body
	var lines []string
	for _, p := range paras {
		lines = append(lines, p...)
	}
	out := joinLines(lines)
	if out == "" {
		return "", &ExtractError{"document contains no extractable text"}
	}
	return out, nil
}

func docxParagraphs(doc []byte) ([][]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var paras [][]string
	var cur strings.Builder
	var curPara []string
	inP := false
	flush := func() {
		if inP {
			paras = append(paras, append([]string{}, curPara...))
			curPara = nil
			cur.Reset()
			inP = false
		}
	}
	_ = flush
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, &ExtractError{fmt.Sprintf("malformed document: %v", err)}
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			switch name {
			case "p":
				if inP {
					paras = append(paras, append([]string{}, curPara...))
					curPara = nil
					cur.Reset()
				}
				inP = true
			case "tab":
				if inP {
					curPara = append(curPara, cur.String()+"\t")
					cur.Reset()
				}
			case "br", "cr":
				if inP {
					curPara = append(curPara, cur.String())
					cur.Reset()
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				if inP {
					curPara = append(curPara, cur.String())
					paras = append(paras, append([]string{}, curPara...))
					curPara = nil
					cur.Reset()
					inP = false
				}
			case "r":
				if inP && cur.Len() > 0 {
					curPara = append(curPara, cur.String())
					cur.Reset()
				}
			}
		case xml.CharData:
			if inP {
				cur.Write([]byte(t))
			}
		}
	}
	var lines []string
	for _, p := range paras {
		lines = append(lines, strings.Join(p, ""))
	}
	return [][]string{lines}, nil
}

// --- XLSX ---

func extractXLSX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", &ExtractError{fmt.Sprintf("not a valid spreadsheet: %v", err)}
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "xl/worksheets/") || f.Name == "xl/workbook.xml" || f.Name == "xl/sharedStrings.xml" {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(rc, maxDocumentBytes))
			rc.Close()
			if err != nil {
				continue
			}
			files[f.Name] = b
		}
	}
	wb, ok := files["xl/workbook.xml"]
	if !ok {
		return "", &ExtractError{"not a valid spreadsheet: missing xl/workbook.xml"}
	}
	sheets, err := xlsxSheets(wb)
	if err != nil {
		return "", err
	}
	var shared []string
	if ss, ok := files["xl/sharedStrings.xml"]; ok {
		shared, _ = xlsxSharedStrings(ss)
	}
	var out []string
	for _, sh := range sheets {
		if sh.hidden {
			continue
		}
		part := "xl/worksheets/" + sh.part
		raw, ok := files[part]
		if !ok {
			// Try with sheet path as stored.
			found := false
			for name, b := range files {
				if strings.HasSuffix(name, "/"+sh.part) {
					raw, ok, found = b, true, true
					_ = found
					break
				}
			}
			if !ok {
				continue
			}
		}
		rows, err := xlsxRows(raw, shared)
		if err != nil {
			continue
		}
		out = append(out, "# -- Sheet: "+sh.name+" --")
		if len(rows) == 0 {
			out = append(out, "(empty)")
		} else {
			for _, r := range rows {
				out = append(out, strings.Join(r, "\t"))
			}
		}
		out = append(out, "")
	}
	text := joinLines(out)
	if text == "" {
		return "", &ExtractError{"spreadsheet has no visible sheets with content"}
	}
	return text, nil
}

type xlsxSheet struct {
	name   string
	part   string
	hidden bool
}

func xlsxSheets(wb []byte) ([]xlsxSheet, error) {
	dec := xml.NewDecoder(bytes.NewReader(wb))
	var sheets []xlsxSheet
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, &ExtractError{fmt.Sprintf("malformed spreadsheet: %v", err)}
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "sheet" {
			continue
		}
		var name, state, rid string
		for _, a := range se.Attr {
			switch a.Name.Local {
			case "name":
				name = a.Value
			case "state":
				state = a.Value
			case "id":
				rid = a.Value
			}
		}
		_ = rid
		part := "sheet" + strconv.Itoa(len(sheets)+1) + ".xml"
		sheets = append(sheets, xlsxSheet{name: name, part: part, hidden: state == "hidden" || state == "veryHidden"})
	}
	if len(sheets) == 0 {
		return nil, &ExtractError{"spreadsheet has no visible sheets with content"}
	}
	return sheets, nil
}

func xlsxSharedStrings(ss []byte) ([]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(ss))
	var out []string
	var cur strings.Builder
	inT := false
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				inT = true
				cur.Reset()
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "si":
				out = append(out, cur.String())
				cur.Reset()
			}
		case xml.CharData:
			if inT {
				cur.Write([]byte(t))
			}
		}
	}
	return out, nil
}

func xlsxRows(raw []byte, shared []string) ([][]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var rows [][]string
	var curRow map[int]string
	maxCol := -1
	flushRow := func() {
		if curRow == nil {
			return
		}
		row := make([]string, maxCol+1)
		for i := range row {
			row[i] = curRow[i]
		}
		rows = append(rows, row)
		curRow = nil
		maxCol = -1
	}
	var curCell struct {
		ref    string
		typ    string
		value  strings.Builder
		text   strings.Builder
		inV    bool
		inT    bool
		active bool
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				if len(rows) >= maxXLSXRows {
					return rows, nil
				}
				curRow = map[int]string{}
			case "c":
				curCell = struct {
					ref    string
					typ    string
					value  strings.Builder
					text   strings.Builder
					inV    bool
					inT    bool
					active bool
				}{active: true}
				for _, a := range t.Attr {
					if a.Name.Local == "r" {
						curCell.ref = a.Value
					}
					if a.Name.Local == "t" {
						curCell.typ = a.Value
					}
				}
			case "v":
				curCell.inV = true
				curCell.value.Reset()
			case "t":
				curCell.inT = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "row":
				flushRow()
			case "v":
				curCell.inV = false
			case "t":
				curCell.inT = false
			case "c":
				if curCell.active && curRow != nil {
					col := maxCol + 1
					if curCell.ref != "" {
						col = colIndex(curCell.ref)
					}
					if col >= 0 && col < maxXLSXCols {
						val := curCell.value.String()
						switch curCell.typ {
						case "s":
							if i, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && i >= 0 && i < len(shared) {
								val = shared[i]
							} else {
								val = ""
							}
						case "inlineStr":
							val = curCell.text.String()
						case "b":
							if v := strings.TrimSpace(val); v == "1" || strings.EqualFold(v, "true") {
								val = "TRUE"
							} else {
								val = "FALSE"
							}
						case "e":
							val = "#ERROR"
						}
						curRow[col] = val
						if col > maxCol {
							maxCol = col
						}
					}
				}
				curCell.active = false
			}
		case xml.CharData:
			if curCell.inV {
				curCell.value.Write([]byte(t))
			}
			if curCell.inT {
				curCell.text.Write([]byte(t))
			}
		}
	}
	for len(rows) > 0 {
		blank := true
		for _, v := range rows[len(rows)-1] {
			if strings.TrimSpace(v) != "" {
				blank = false
				break
			}
		}
		if !blank {
			break
		}
		rows = rows[:len(rows)-1]
	}
	return rows, nil
}

func colIndex(ref string) int {
	n := 0
	for _, r := range ref {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			break
		}
		up := r
		if up >= 'a' {
			up = up - 'a' + 'A'
		}
		n = n*26 + int(up-'A'+1)
	}
	return n - 1
}

// --- PDF (best-effort text layer) ---

var pdfPageRe = regexp.MustCompile(`/Type\s*/Page[^s]`)

func extractPDF(data []byte, filename string) (string, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("%PDF")) {
		// Lenient: some producers emit a BOM or leading whitespace; require the marker nearby.
		if idx := bytes.Index(data[:minInt(1024, len(data))], []byte("%PDF")); idx < 0 {
			return "", &ExtractError{"not a valid PDF"}
		}
	}
	if text, err := pdftotextExtract(data); err == nil {
		return finalizePDFText(data, text)
	}
	text := pdfTextLayer(data)
	text = strings.TrimSpace(text)
	if text == "" {
		return "", &ExtractError{"document contains no extractable text (scanned pages need OCR: render with pdftoppm and inspect via vision_analyze)"}
	}
	if looksGarbled(text) {
		return "", &ExtractError{"document text could not be decoded (custom font encoding); convert with pdftotext or a document tool and retry"}
	}
	return finalizePDFText(data, text)
}

// finalizePDFText applies the empty and coverage checks shared by both PDF paths.
func finalizePDFText(data []byte, text string) (string, error) {
	pages := len(pdfPageRe.FindAll(data, -1))
	if pages < 1 {
		pages = 1
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", &ExtractError{"document contains no extractable text (scanned pages need OCR: render with pdftoppm and inspect via vision_analyze)"}
	}
	if pages >= 2 && len([]rune(text)) < pages*20 {
		warn := "[EXTRACTION COVERAGE WARNING: only part of this PDF yielded text. " +
			"Unreadable pages are likely scanned images — their content is MISSING below. " +
			"For the gaps that matter, render that range with `pdftoppm -jpeg -r 150 -f <first> -l <last> <file> $TMPDIR/page` " +
			"and inspect each image with vision_analyze.]\n"
		return warn + text + "\n", nil
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text, nil
}

// looksGarbled reports whether extracted text is likely misdecoded bytes
// rather than readable content: too few tokens carry real letter runs.
func looksGarbled(text string) bool {
	fields := strings.Fields(text)
	if len(fields) < 10 {
		return false
	}
	withLetters := 0
	for _, f := range fields {
		letters := 0
		for _, r := range f {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				letters++
			}
		}
		if letters >= 2 {
			withLetters++
		}
	}
	return withLetters*4 < len(fields)
}

// pdftotextExtract renders the PDF text layer with the poppler binary when
// installed. The bytes spill to a private temp file removed on return.
func pdftotextExtract(data []byte) (string, error) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "oswald-pdf-*.pdf")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-layout", "-eol", "unix", "-q", "--", name, "-")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if out.Len() == 0 || out.Len() > maxDocumentBytes {
		return "", errors.New("pdftotext produced no usable output")
	}
	return out.String(), nil
}

func pdfTextLayer(data []byte) string {
	var sb strings.Builder
	rest := data
	for {
		i := bytes.Index(rest, []byte("stream"))
		if i < 0 {
			break
		}
		start := i + len("stream")
		for start < len(rest) && (rest[start] == '\r' || rest[start] == '\n') {
			start++
		}
		end := bytes.Index(rest[start:], []byte("endstream"))
		if end < 0 {
			break
		}
		raw := rest[start : start+end]
		raw = decodeStream(raw, streamDict(rest, i))
		if raw != nil {
			sb.WriteString(pdfStrings(raw))
			sb.WriteString("\n")
		}
		rest = rest[start+end+len("endstream"):]
		if sb.Len() > maxDocumentBytes {
			break
		}
	}
	return collapsePDFText(sb.String())
}

// streamDict returns the dictionary text preceding a stream keyword so the
// caller can honor declared filters. Only the tail is inspected: the dict
// directly abuts its stream.
func streamDict(rest []byte, at int) string {
	start := at - 1000
	if start < 0 {
		start = 0
	}
	return string(rest[start:at])
}

// decodeStream applies declared stream filters. Undecodable filtered bytes
// are refused (nil) rather than scanned raw: compressed data reads as text
// garbage. Filterless streams still try inflate for unlabeled deflate.
func decodeStream(raw []byte, dict string) []byte {
	hasFlate := strings.Contains(dict, "Flate") || strings.Contains(dict, "/Fl ")
	hasASCII85 := strings.Contains(dict, "ASCII85") || strings.Contains(dict, "/A85")
	if hasASCII85 {
		decoded := tryASCII85(raw)
		if decoded == nil {
			if hasFlate {
				return nil
			}
		} else {
			raw = decoded
		}
	}
	if hasFlate {
		return tryInflate(raw)
	}
	if inflated := tryInflate(raw); inflated != nil {
		return inflated
	}
	return raw
}

// tryASCII85 decodes whitespace-stripped Adobe ASCII85, with or without markers.
func tryASCII85(raw []byte) []byte {
	clean := bytes.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, raw)
	clean = bytes.TrimPrefix(clean, []byte("<~"))
	clean = bytes.TrimSuffix(clean, []byte("~>"))
	if len(clean) == 0 {
		return nil
	}
	dst := make([]byte, len(clean))
	n, _, err := ascii85.Decode(dst, clean, true)
	if err != nil {
		return nil
	}
	return dst[:n]
}

func tryInflate(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxDocumentBytes+1))
	if err != nil || len(out) == 0 {
		return nil
	}
	return out
}

var pdfParenRe = regexp.MustCompile(`\((?:\\.|[^\\()])*\)`)
var pdfHexRe = regexp.MustCompile(`<([0-9A-Fa-f\s]+)>`)

// decodeUTF16 decodes BOM-marked UTF-16 bytes, returning ok=false otherwise.
func decodeUTF16(b []byte) (string, bool) {
	if len(b) < 4 || len(b)%2 != 0 {
		return "", false
	}
	var bo binary.ByteOrder = binary.BigEndian
	switch {
	case b[0] == 0xFE && b[1] == 0xFF:
		b = b[2:]
	case b[0] == 0xFF && b[1] == 0xFE:
		bo = binary.LittleEndian
		b = b[2:]
	default:
		return "", false
	}
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		u16 = append(u16, bo.Uint16(b[i:]))
	}
	return string(utf16.Decode(u16)), true
}

func pdfStrings(raw []byte) string {
	var parts []string
	for _, m := range pdfParenRe.FindAll(raw, -1) {
		inner := string(m[1 : len(m)-1])
		inner = strings.ReplaceAll(inner, `\)`, ")")
		inner = strings.ReplaceAll(inner, `\(`, "(")
		inner = strings.ReplaceAll(inner, `\\`, "\\")
		inner = strings.ReplaceAll(inner, `\n`, "\n")
		inner = strings.ReplaceAll(inner, `\r`, "\n")
		if strings.TrimSpace(inner) != "" {
			parts = append(parts, inner)
		}
	}
	for _, m := range pdfHexRe.FindAllSubmatch(raw, -1) {
		hexStr := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, string(m[1]))
		if len(hexStr) == 0 || len(hexStr) > 4096 {
			continue
		}
		if len(hexStr)%2 == 1 {
			hexStr += "0"
		}
		decoded := make([]byte, 0, len(hexStr)/2)
		ok := true
		for i := 0; i < len(hexStr); i += 2 {
			v, err := strconv.ParseUint(hexStr[i:i+2], 16, 8)
			if err != nil {
				ok = false
				break
			}
			decoded = append(decoded, byte(v))
		}
		if !ok {
			continue
		}
		if s, ok := decodeUTF16(decoded); ok {
			decoded = []byte(s)
		}
		s := string(decoded)
		if !utf8.ValidString(s) || strings.TrimSpace(s) == "" {
			continue
		}
		printable := 0
		for _, r := range s {
			if r >= 32 || r == '\n' || r == '\t' {
				printable++
			}
		}
		if printable*2 < len([]rune(s)) {
			continue
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func collapsePDFText(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

func joinLines(lines []string) string {
	var kept []string
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		kept = append(kept, ln)
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
