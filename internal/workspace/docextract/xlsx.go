// Package docextract converts inbound documents to readable text.
package docextract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ExtractError marks a supported-looking document that cannot render as text.

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
