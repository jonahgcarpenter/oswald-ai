package routing

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// IsDocumentAttachment classifies supported current uploads without downloading them.
func IsDocumentAttachment(filename, mediaType string) bool {
	if media.LooksLikeImageMIME(mediaType) {
		return false
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".pdf", ".txt", ".text", ".md", ".markdown", ".csv", ".tsv", ".doc", ".docx", ".xlsx", ".ods", ".ppt", ".pptx", ".odp", ".html", ".htm", ".rtf", ".odt", ".json", ".xml",
		".log", ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".c", ".h", ".cpp", ".rs", ".java", ".sh", ".sql", ".yaml", ".yml", ".toml", ".ini", ".css":
		return true
	}
	if filepath.Ext(filename) != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0])) {
	case "application/pdf", "text/plain", "text/markdown", "text/csv", "text/tab-separated-values", "application/json", "application/xml", "text/xml", "text/html", "text/rtf", "application/rtf", "application/msword", "application/vnd.ms-powerpoint", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "application/vnd.oasis.opendocument.text", "application/vnd.oasis.opendocument.spreadsheet", "application/vnd.oasis.opendocument.presentation":
		return true
	}
	return false
}

// DocumentDownload holds transport metadata only. URLs and keys must never be logged.
type DocumentDownload struct {
	Filename, MediaType, AdmissionKey, URL string
	Size                                   int64
}

// NewDocumentLoader enforces byte limits while streaming and validates every redirect
// before sending credentials. The supplied client is copied, never mutated.
func NewDocumentLoader(client *http.Client, docs []DocumentDownload, allowed func(*url.URL) bool) *requestctx.DocumentLoader {
	if len(docs) == 0 {
		return nil
	}
	docs = append([]DocumentDownload(nil), docs...)
	var sourceBytes int64
	var boundsErr error
	if len(docs) > 4 {
		boundsErr = fmt.Errorf("at most four documents may be uploaded at once")
	}
	for i := range docs {
		if docs[i].Size <= 0 {
			docs[i].Size = 20 << 20
		}
		if docs[i].Size > 20<<20 {
			boundsErr = fmt.Errorf("document exceeds 20 MiB")
		}
		// Saturate invalid metadata rather than risk overflowing an untrusted sum.
		sourceBytes = min(41<<20, sourceBytes+min(docs[i].Size, 41<<20))
	}
	if sourceBytes > 40<<20 {
		boundsErr = fmt.Errorf("document upload byte limit exceeded")
	}
	return &requestctx.DocumentLoader{FileCount: len(docs), SourceBytes: sourceBytes, Load: func(ctx context.Context) ([]requestctx.DocumentUpload, error) {
		if boundsErr != nil {
			return nil, boundsErr
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		c := *client
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 4 || !allowed(req.URL) {
				return fmt.Errorf("document redirect rejected")
			}
			return nil
		}
		total := 0
		uploads := make([]requestctx.DocumentUpload, 0, len(docs))
		for _, doc := range docs {
			if doc.Size > 20<<20 {
				return nil, fmt.Errorf("document exceeds 20 MiB")
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, doc.URL, nil)
			if err != nil || !allowed(req.URL) {
				return nil, fmt.Errorf("document URL rejected")
			}
			resp, err := c.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, fmt.Errorf("document download failed")
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				resp.Body.Close()
				return nil, fmt.Errorf("document download status %d", resp.StatusCode)
			}
			bound := min(int(doc.Size), (40<<20)-total)
			data, err := io.ReadAll(io.LimitReader(resp.Body, int64(bound)+1))
			resp.Body.Close()
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, fmt.Errorf("document download interrupted")
			}
			if len(data) > bound {
				return nil, fmt.Errorf("document upload byte limit exceeded")
			}
			total += len(data)
			if strings.TrimSpace(doc.MediaType) == "" {
				doc.MediaType = mime.TypeByExtension(strings.ToLower(filepath.Ext(doc.Filename)))
				if doc.MediaType == "" {
					doc.MediaType = "application/octet-stream"
				}
			}
			uploads = append(uploads, requestctx.DocumentUpload{Filename: doc.Filename, MediaType: doc.MediaType, AdmissionKey: doc.AdmissionKey, Data: data})
		}
		return uploads, nil
	}}
}
