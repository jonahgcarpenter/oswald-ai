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

// DocumentRef is a gateway-neutral reference to a cached inbound file:
// documents and motion media (GIF/video) share the lane. Bytes stay in the
// private cache; prompts and history carry only the path. Data is transient
// upload bytes, never persisted. Motion metadata (duration and dimensions)
// is best-effort and zero when ffprobe is unavailable.
type DocumentRef struct {
	Path            string  `json:"path"`
	Filename        string  `json:"filename"`
	MIMEType        string  `json:"mime_type,omitempty"`
	Size            int64   `json:"size"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Width           int     `json:"width,omitempty"`
	Height          int     `json:"height,omitempty"`
	Data            []byte  `json:"-"`
}

// IsMotion reports whether the ref names inbound GIF or video media.
func (d DocumentRef) IsMotion() bool {
	return LooksLikeMotionMIME(d.MIMEType) || IsMotionExtension(d.Filename)
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

// DocumentContextNote renders the current-turn prompt note for one inbound
// file, mirroring the image annotation: a stable cache path plus the original
// filename. Motion media adds duration and dimensions when ffprobe found
// them. No read instructions ride along; the tool catalog owns them.
func DocumentContextNote(doc DocumentRef) string {
	if doc.IsMotion() {
		return MotionContextNote(doc)
	}
	name := strings.TrimSpace(doc.Filename)
	if name == "" {
		name = "document"
	}
	return fmt.Sprintf("[File attached at: %s ('%s')]", strings.TrimSpace(doc.Path), name)
}

// MotionContextNote renders the prompt annotation for one cached GIF/video:
// the path the agent passes to ffmpeg, plus best-effort duration/dimensions.
func MotionContextNote(doc DocumentRef) string {
	name := strings.TrimSpace(doc.Filename)
	if name == "" {
		name = "motion"
	}
	kind := "Video"
	if normalizeMIMEType(doc.MIMEType) == "image/gif" || strings.EqualFold(filepath.Ext(name), ".gif") {
		kind = "GIF"
	}
	meta := make([]string, 0, 2)
	if doc.DurationSeconds > 0 {
		total := int(doc.DurationSeconds + 0.5)
		meta = append(meta, fmt.Sprintf("%d:%02d", total/60, total%60))
	}
	if doc.Width > 0 && doc.Height > 0 {
		meta = append(meta, fmt.Sprintf("%d×%d", doc.Width, doc.Height))
	}
	detail := ""
	if len(meta) > 0 {
		detail = ", " + strings.Join(meta, ", ")
	}
	return fmt.Sprintf("[%s attached at: %s ('%s'%s)]", kind, strings.TrimSpace(doc.Path), name, detail)
}

// MotionRecipeNote tells the model how to inspect motion media itself: the
// sandbox has ffmpeg/ffprobe and vision_analyze accepts workspace frames.
const MotionRecipeNote = "[Motion media is not inlined: extract frames from the attached cache path with terminal ffmpeg into cache/scratch (for example: ffmpeg -i <path> -vf fps=1/2 ~/cache/scratch/frame-%d.jpg), then vision_analyze the frames; ffprobe gives codec and timing details.]"

// DocumentsNote joins per-file notes for the current turn, appending the
// ffmpeg recipe once when any motion media is present.
func DocumentsNote(docs []DocumentRef) string {
	parts := make([]string, 0, len(docs))
	motion := false
	for _, doc := range docs {
		if strings.TrimSpace(doc.Path) == "" {
			continue
		}
		if doc.IsMotion() {
			motion = true
		}
		parts = append(parts, DocumentContextNote(doc))
	}
	if motion {
		parts = append(parts, MotionRecipeNote)
	}
	return strings.Join(parts, "\n")
}
