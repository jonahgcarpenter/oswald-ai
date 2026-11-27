// Package docextract converts inbound documents to readable text.
package docextract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ExtractError marks a supported-looking document that cannot render as text.

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
