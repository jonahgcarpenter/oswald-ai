package documents

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

func extractText(ctx context.Context, ext string, data []byte, r *Result) error {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return ErrInvalid
	}
	switch ext {
	case ".txt", ".text", ".md", ".markdown", ".log", ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".rs", ".java", ".sh", ".bash", ".zsh", ".sql", ".yaml", ".yml", ".toml", ".ini", ".css", ".scss", ".rb", ".php", ".cs", ".swift", ".kt", ".scala", ".r", ".pl", ".lua", ".ps1", ".conf", ".properties":
		appendText(r, string(data), "document", "native")
	case ".json":
		if !json.Valid(data) {
			return ErrInvalid
		}
		appendText(r, string(data), "document", "native")
	case ".csv", ".tsv":
		separator := byte(',')
		if ext == ".tsv" {
			separator = '\t'
		}
		if err := preflightCSV(ctx, data, separator); err != nil {
			return err
		}
		reader := csv.NewReader(bytes.NewReader(data))
		reader.FieldsPerRecord = -1
		if ext == ".tsv" {
			reader.Comma = '\t'
		}
		for row := 1; ; row++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			record, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return ErrInvalid
			}
			if row > 10000 {
				r.Partial = true
				break
			}
			if !appendText(r, strings.Join(record, "\t"), fmt.Sprintf("row:%d", row), "native") {
				r.Partial = true
				break
			}
		}
	case ".xml":
		if err := validateXML(ctx, data); err != nil {
			return err
		}
		d := xml.NewDecoder(bytes.NewReader(data))
		var out strings.Builder
		for {
			t, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return ErrInvalid
			}
			if v, ok := t.(xml.CharData); ok {
				out.Write(v)
				out.WriteByte(' ')
			}
			if out.Len() > maxText {
				r.Partial = true
				break
			}
		}
		appendText(r, strings.TrimSpace(out.String()), "document", "native")
	case ".html", ".htm":
		z := html.NewTokenizer(bytes.NewReader(data))
		var out strings.Builder
		hidden := 0
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			t := z.Next()
			if t == html.ErrorToken {
				if z.Err() != io.EOF {
					return ErrInvalid
				}
				break
			}
			if t == html.StartTagToken {
				name, _ := z.TagName()
				s := string(name)
				if s == "script" || s == "style" || s == "template" {
					hidden++
				}
			}
			if t == html.EndTagToken {
				name, _ := z.TagName()
				s := string(name)
				if hidden > 0 && (s == "script" || s == "style" || s == "template") {
					hidden--
				}
				if hidden == 0 {
					out.WriteByte('\n')
				}
			}
			if t == html.TextToken && hidden == 0 {
				out.Write(z.Text())
				out.WriteByte(' ')
			}
			if out.Len() > maxText {
				r.Partial = true
				break
			}
		}
		appendText(r, strings.TrimSpace(out.String()), "document", "native")
	default:
		return ErrUnsupported
	}
	return nil
}

const (
	maxCSVRecordBytes = 256 << 10
	maxCSVFields      = 1024
)

// csv.Reader allocates field indexes and positions before returning a record.
// Check the complete input first, without allocating fields or copying text.
// Limits count raw bytes (including quotes/newlines) and parsed field boundaries,
// so quoted separators and embedded newlines cannot bypass or inflate them.
func preflightCSV(ctx context.Context, data []byte, separator byte) error {
	const (
		fieldStart = iota
		unquoted
		quoted
		afterQuote
	)
	state, fields, size := fieldStart, 1, 0
	for i, c := range data {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		size++
		if size > maxCSVRecordBytes {
			return ErrLimit
		}
		if state == quoted {
			if c == '"' {
				state = afterQuote
			}
			continue
		}
		if state == afterQuote && c == '"' {
			state = quoted
			continue
		}
		// encoding/csv normalizes CRLF and drops a final CR at EOF. A bare CR
		// elsewhere remains field data and is invalid after a closing quote.
		if c == '\r' && (i+1 == len(data) || data[i+1] == '\n') {
			continue
		}
		if c == '\n' {
			state, fields, size = fieldStart, 1, 0
			continue
		}
		if c == separator {
			fields++
			if fields > maxCSVFields {
				return ErrLimit
			}
			state = fieldStart
			continue
		}
		switch state {
		case fieldStart:
			if c == '"' {
				state = quoted
			} else {
				state = unquoted
			}
		case unquoted:
			if c == '"' {
				return ErrInvalid
			}
		case afterQuote:
			return ErrInvalid
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if state == quoted {
		return ErrInvalid
	}
	return nil
}

// Reject DTDs/entities and excessive nesting before any structured decoding.
func validateXML(ctx context.Context, data []byte) error {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	d := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	roots := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, err := d.Token()
		if err == io.EOF {
			if roots != 1 {
				return ErrInvalid
			}
			return nil
		}
		if err != nil {
			return ErrInvalid
		}
		switch v := t.(type) {
		case xml.Directive:
			return ErrInvalid
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return ErrInvalid
				}
			}
			depth++
			if depth > 64 {
				return ErrLimit
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(v)) != "" {
				return ErrInvalid
			}
		}
	}
}
