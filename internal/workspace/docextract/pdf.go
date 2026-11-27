// Package docextract converts inbound documents to readable text.
package docextract

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/ascii85"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// ExtractError marks a supported-looking document that cannot render as text.

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
