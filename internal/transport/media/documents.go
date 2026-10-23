package media

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
)

const (
	// MaxDocumentsPerRequest bounds document fan-in per request.
	MaxDocumentsPerRequest = 4
	// MaxDocumentBytes limits each decoded document payload accepted from a gateway.
	MaxDocumentBytes = 10 << 20
	// MaxTotalDocumentBytes bounds a whole admission, not each file.
	MaxTotalDocumentBytes = 30 << 20
)

// DocumentRef is a gateway-neutral reference to a cached inbound document.
// Bytes stay in the private cache; prompts and history carry only the path.
// Data is transient upload bytes, never persisted.
type DocumentRef struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type,omitempty"`
	Size     int64  `json:"size"`
	Data     []byte `json:"-"`
}

// DocumentLabel returns a readable label for prompt notes and fallbacks.
func DocumentLabel(name, mimeType string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	mimeType = strings.TrimSpace(mimeType)
	if mimeType != "" {
		return mimeType
	}
	return "unknown file"
}

var extractableDocumentExtensions = map[string]struct{}{
	".pdf":  {},
	".txt":  {},
	".md":   {},
	".csv":  {},
	".log":  {},
	".json": {},
	".docx": {},
	".xlsx": {},
}

// IsExtractableDocument reports whether name has an extractable document extension.
func IsExtractableDocument(name string) bool {
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(name)))
	_, ok := extractableDocumentExtensions[ext]
	return ok
}

// LooksLikeDocumentMIME reports whether mimeType is a plausible document type.
// Empty stays accepted so extension sniffing can decide.
func LooksLikeDocumentMIME(mimeType string) bool {
	mimeType = strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	if mimeType == "" {
		return true
	}
	if strings.HasPrefix(mimeType, "image/") || strings.HasPrefix(mimeType, "audio/") || strings.HasPrefix(mimeType, "video/") {
		return false
	}
	return true
}

// SniffDocument validates raw bytes against the claimed name and returns the
// effective filename, MIME type, and size.
func SniffDocument(name, declaredMIME string, data []byte) (string, string, error) {
	if len(data) == 0 || len(data) > MaxDocumentBytes {
		return "", "", fmt.Errorf("document size out of bounds")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "document"
	}
	base := filepath.Base(name)
	if base == "." || base == "/" || base == "" {
		base = "document"
	}
	mime := strings.TrimSpace(strings.ToLower(strings.Split(declaredMIME, ";")[0]))
	sniffed := http.DetectContentType(data)
	if strings.HasPrefix(sniffed, "image/") || strings.HasPrefix(sniffed, "audio/") || strings.HasPrefix(sniffed, "video/") {
		return "", "", fmt.Errorf("unsupported document media type")
	}
	ext := strings.ToLower(filepath.Ext(base))
	if _, ok := extractableDocumentExtensions[ext]; !ok {
		if strings.HasPrefix(sniffed, "text/") && ext == "" {
			base += ".txt"
		} else {
			return "", "", fmt.Errorf("unsupported document type")
		}
	}
	if mime == "" || mime == "application/octet-stream" {
		mime = sniffed
		if strings.HasSuffix(strings.ToLower(base), ".pdf") && !strings.Contains(mime, "pdf") {
			mime = "application/pdf"
		}
	}
	return base, mime, nil
}

// DocumentContextNote renders the current-turn prompt note for one document.
// Text documents name the file; binary formats direct the model to extract.
func DocumentContextNote(doc DocumentRef) string {
	name := strings.TrimSpace(doc.Filename)
	if name == "" {
		name = "document"
	}
	path := strings.TrimSpace(doc.Path)
	mime := strings.ToLower(strings.TrimSpace(doc.MIMEType))
	if strings.HasPrefix(mime, "text/") {
		return fmt.Sprintf("[The user sent a text document: '%s'. It is saved at: %s. Read it with read_file before answering when the request involves its contents.]", name, path)
	}
	return fmt.Sprintf("[The user sent a document: '%s'. It is saved at: %s. Its text is not inlined (binary format). Extract the document text with read_file before answering, instead of asking the user to paste the contents.]", name, path)
}

// DocumentsNote joins per-document notes for the current turn.
func DocumentsNote(docs []DocumentRef) string {
	parts := make([]string, 0, len(docs))
	for _, doc := range docs {
		if strings.TrimSpace(doc.Path) == "" {
			continue
		}
		parts = append(parts, DocumentContextNote(doc))
	}
	return strings.Join(parts, "\n")
}
