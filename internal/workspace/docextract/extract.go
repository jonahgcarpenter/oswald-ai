// Package docextract converts inbound documents to readable text.
package docextract

import (
	"fmt"
	"path/filepath"
	"strings"
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
