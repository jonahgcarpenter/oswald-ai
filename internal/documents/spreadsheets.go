package documents

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

const maxCells = 10000

type richString struct {
	Text string `xml:"t"`
	Runs []struct {
		Text string `xml:"t"`
	} `xml:"r"`
}

func (s richString) text() string {
	var b strings.Builder
	b.WriteString(s.Text)
	for _, r := range s.Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

func spreadsheet(ctx context.Context, ext string, data []byte, r *Result) error {
	files, err := openArchive(ctx, data)
	if err != nil {
		return err
	}
	if ext == ".ods" {
		return ods(ctx, files["content.xml"], r)
	}
	var book struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			ID   string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	var rels struct {
		Items []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
			Type   string `xml:"Type,attr"`
		} `xml:"Relationship"`
	}
	if xml.Unmarshal(files["xl/workbook.xml"], &book) != nil || xml.Unmarshal(files["xl/_rels/workbook.xml.rels"], &rels) != nil {
		return ErrInvalid
	}
	var shared struct {
		Items []richString `xml:"si"`
	}
	if b, ok := files["xl/sharedStrings.xml"]; ok {
		if xml.Unmarshal(b, &shared) != nil {
			return ErrInvalid
		}
	}
	targets := make(map[string]string)
	skipped := make(map[string]bool)
	for _, rel := range rels.Items {
		if strings.HasSuffix(rel.Type, "/chartsheet") {
			skipped[rel.ID] = true
			continue
		}
		if rel.Mode == "External" || !strings.HasSuffix(rel.Type, "/worksheet") {
			continue
		}
		target := path.Join("xl", rel.Target)
		if strings.HasPrefix(rel.Target, "/") {
			target = strings.TrimPrefix(rel.Target, "/")
		}
		if !strings.HasPrefix(target, "xl/worksheets/") || strings.ContainsAny(target, "\\:\x00") || targets[rel.ID] != "" {
			return ErrInvalid
		}
		targets[rel.ID] = target
	}
	cells := 0
	for i, sheet := range book.Sheets {
		if i >= 100 {
			r.Partial = true
			break
		}
		if skipped[sheet.ID] {
			r.Partial = true
			continue
		}
		b, ok := files[targets[sheet.ID]]
		if !ok {
			return ErrInvalid
		}
		d := xml.NewDecoder(bytes.NewReader(b))
		row, col := 0, 0
		explicitRow := false
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			t, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return ErrInvalid
			}
			s, ok := t.(xml.StartElement)
			if !ok {
				continue
			}
			if s.Name.Local == "row" {
				row++
				col = 0
				explicitRow = attr(s, "r") != ""
				if n := attr(s, "r"); n != "" {
					row, err = strconv.Atoi(n)
					if err != nil || row < 1 || row > 1048576 {
						return ErrInvalid
					}
				}
			}
			if s.Name.Local != "c" {
				continue
			}
			cells++
			if cells > maxCells {
				r.Partial = true
				return nil
			}
			var c struct {
				Ref     string     `xml:"r,attr"`
				Type    string     `xml:"t,attr"`
				Value   *string    `xml:"v"`
				Inline  richString `xml:"is"`
				Formula *string    `xml:"f"`
			}
			if d.DecodeElement(&c, &s) != nil {
				return ErrInvalid
			}
			col++
			if c.Ref != "" {
				var cellRow int
				firstCell := col == 1
				col, cellRow, err = cellReference(c.Ref)
				if !explicitRow && firstCell {
					row = cellRow
				}
				if err != nil || cellRow != row {
					return ErrInvalid
				}
			}
			if row < 1 || col > 16384 {
				return ErrInvalid
			}
			value := ""
			if c.Value != nil {
				value = *c.Value
			}
			missingCache := c.Formula != nil && c.Value == nil
			if !missingCache {
				switch c.Type {
				case "s":
					index, err := strconv.Atoi(strings.TrimSpace(value))
					if err != nil || index < 0 || index >= len(shared.Items) {
						return ErrInvalid
					}
					value = shared.Items[index].text()
				case "inlineStr":
					value = c.Inline.text()
				case "b":
					if value == "1" {
						value = "true"
					} else if value == "0" {
						value = "false"
					} else {
						return ErrInvalid
					}
				case "", "n", "str", "e", "d": // Numeric serials stay raw; styles/dates are not guessed.
				default:
					return ErrInvalid
				}
			}
			if c.Formula != nil {
				if missingCache {
					value = "[formula: no cached value]"
					r.Partial = true
				} else {
					value += " [cached formula value]"
				}
			}
			if value == "" {
				continue
			}
			locator := fmt.Sprintf("sheet:%d row:%d column:%d", i+1, row, col)
			if !appendText(r, value, locator, "native") {
				r.Partial = true
				return nil
			}
		}
	}
	return nil
}

func cellReference(ref string) (col, row int, err error) {
	i := 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		col = col*26 + int(ref[i]-'A') + 1
		i++
		if col > 16384 {
			return 0, 0, ErrInvalid
		}
	}
	if i == 0 || i == len(ref) {
		return 0, 0, ErrInvalid
	}
	row, err = strconv.Atoi(ref[i:])
	if err != nil || row < 1 || row > 1048576 {
		return 0, 0, ErrInvalid
	}
	return
}

func attr(s xml.StartElement, key string) string {
	for _, a := range s.Attr {
		if a.Name.Local == key {
			return a.Value
		}
	}
	return ""
}

func repeat(s xml.StartElement, key string) (int, error) {
	v := attr(s, key)
	if v == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 1048576 {
		return 0, ErrLimit
	}
	return n, nil
}

func ods(ctx context.Context, data []byte, r *Result) error {
	if len(data) == 0 {
		return ErrInvalid
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	sheet, row, cells := 0, 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return ErrInvalid
		}
		s, ok := t.(xml.StartElement)
		if !ok {
			continue
		}
		if s.Name.Local == "table" {
			sheet++
			row = 0
			if sheet > 100 {
				r.Partial = true
				return nil
			}
		}
		if s.Name.Local != "table-row" {
			continue
		}
		if sheet == 0 {
			return ErrInvalid
		}
		repeated, err := repeat(s, "number-rows-repeated")
		if err != nil {
			return err
		}
		// Retain positions without expanding empty trailing rows or allocating a grid.
		type cell struct {
			column int
			count  int
			value  string
		}
		var rowCells []cell
		rowBytes := 0
		column := 1
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			t, err := d.Token()
			if err != nil {
				return ErrInvalid
			}
			if end, ok := t.(xml.EndElement); ok && end.Name.Local == "table-row" {
				break
			}
			cs, ok := t.(xml.StartElement)
			if !ok || (cs.Name.Local != "table-cell" && cs.Name.Local != "covered-table-cell") {
				continue
			}
			count, err := repeat(cs, "number-columns-repeated")
			if err != nil {
				return err
			}
			if column+count > 1048577 {
				return ErrLimit
			}
			value, err := odsCell(d, cs)
			if err != nil {
				return err
			}
			if attr(cs, "formula") != "" && value == "[formula: no cached value]" {
				r.Partial = true
			}
			if cs.Name.Local != "covered-table-cell" && value != "" {
				rowBytes += len(value)
				if rowBytes > maxText {
					return ErrLimit
				}
				rowCells = append(rowCells, cell{column, count, value})
			}
			column += count
			if len(rowCells) > maxCells {
				return ErrLimit
			}
		}
		if row+repeated > 1048576 {
			return ErrLimit
		}
		if len(rowCells) > 0 {
			for rr := 1; rr <= repeated; rr++ {
				for _, c := range rowCells {
					for cc := 0; cc < c.count; cc++ {
						if err := ctx.Err(); err != nil {
							return err
						}
						cells++
						if cells > maxCells {
							r.Partial = true
							return nil
						}
						if !appendText(r, c.value, fmt.Sprintf("sheet:%d row:%d column:%d", sheet, row+rr, c.column+cc), "native") {
							r.Partial = true
							return nil
						}
					}
				}
			}
		}
		row += repeated
	}
}

func odsCell(d *xml.Decoder, start xml.StartElement) (string, error) {
	var text strings.Builder
	depth, paragraph := 1, 0
	for depth > 0 {
		t, err := d.Token()
		if err != nil {
			return "", ErrInvalid
		}
		switch v := t.(type) {
		case xml.StartElement:
			if v.Name.Local == "annotation" {
				if d.Skip() != nil {
					return "", ErrInvalid
				}
				continue
			}
			depth++
			if v.Name.Local == "p" {
				paragraph++
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
			}
			if paragraph > 0 {
				switch v.Name.Local {
				case "s":
					n, err := repeat(v, "c")
					if err != nil || n > 10000 {
						return "", ErrLimit
					}
					text.WriteString(strings.Repeat(" ", n))
				case "tab":
					text.WriteByte('\t')
				case "line-break":
					text.WriteByte('\n')
				}
			}
		case xml.EndElement:
			depth--
			if v.Name.Local == "p" {
				paragraph--
			}
		case xml.CharData:
			if paragraph > 0 {
				text.Write(v)
			}
		}
		if text.Len() > maxText {
			return "", ErrLimit
		}
	}
	value := text.String()
	if value == "" {
		switch attr(start, "value-type") {
		case "string":
			value = attr(start, "string-value")
		case "float", "percentage", "currency":
			value = attr(start, "value")
		case "date":
			value = attr(start, "date-value")
		case "time":
			value = attr(start, "time-value")
		case "boolean":
			value = attr(start, "boolean-value")
		}
	}
	if attr(start, "formula") != "" {
		if value == "" {
			value = "[formula: no cached value]"
		} else {
			value += " [cached formula value]"
		}
	}
	return value, nil
}
