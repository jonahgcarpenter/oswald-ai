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
// documents, media (GIF/video/audio), and catch-all attachments share it.
// Bytes stay in the private cache; prompts and history carry only the path.
// Data is transient upload bytes, never persisted. Media metadata (duration
// and dimensions) is best-effort and zero when ffprobe is unavailable.
type DocumentRef struct {
	Path            string  `json:"path"`
	Filename        string  `json:"filename"`
	MIMEType        string  `json:"mime_type,omitempty"`
	Size            int64   `json:"size"`
	Lane            string  `json:"lane,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Width           int     `json:"width,omitempty"`
	Height          int     `json:"height,omitempty"`
	Data            []byte  `json:"-"`
}

// Lane values for inbound files.
const (
	LaneDocument   = "document"
	LaneMedia      = "media"
	LaneAttachment = "attachment"
)

// IsMedia reports whether the ref names inbound GIF/video/audio media.
func (d DocumentRef) IsMedia() bool {
	if d.Lane != "" {
		return d.Lane == LaneMedia
	}
	return LooksLikeMediaMIME(d.MIMEType) || IsMediaExtension(d.Filename)
}

// IsAttachment reports whether the ref names a catch-all raw attachment.
func (d DocumentRef) IsAttachment() bool {
	return d.Lane == LaneAttachment
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
// file, choosing the lane's annotation: document, AV media, or catch-all
// attachment. Only the path and original name ride along; the tool catalog
// and the media recipe own the instructions.
func DocumentContextNote(doc DocumentRef) string {
	switch {
	case doc.IsMedia():
		return MediaContextNote(doc)
	case doc.IsAttachment():
		return AttachmentContextNote(doc)
	}
	name := strings.TrimSpace(doc.Filename)
	if name == "" {
		name = "document"
	}
	return fmt.Sprintf("[File attached at: %s ('%s')]", strings.TrimSpace(doc.Path), name)
}

// AttachmentContextNote renders the catch-all note: the file exists at a
// cache path the agent can open with terminal tools.
func AttachmentContextNote(doc DocumentRef) string {
	name := strings.TrimSpace(doc.Filename)
	if name == "" {
		name = "attachment"
	}
	return fmt.Sprintf("[Attachment cached at: %s ('%s')]", strings.TrimSpace(doc.Path), name)
}

// MediaContextNote renders the prompt annotation for one cached media file:
// the path the agent passes to ffmpeg, plus best-effort duration/dimensions.
func MediaContextNote(doc DocumentRef) string {
	name := strings.TrimSpace(doc.Filename)
	if name == "" {
		name = "media"
	}
	kind := "Video"
	switch {
	case normalizeMIMEType(doc.MIMEType) == "image/gif" || strings.EqualFold(filepath.Ext(name), ".gif"):
		kind = "GIF"
	case strings.HasPrefix(normalizeMIMEType(doc.MIMEType), "audio/"):
		kind = "Audio"
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

// MediaRecipeNote tells the model how to inspect non-video media itself:
// GIFs through vision_analyze (first frame); audio keeps the terminal ffmpeg
// recipe. Audio is honest about the missing capability — there is no listening
// or transcription tool. Video needs no recipe: video_analyze is
// self-describing in the tool catalog.
const MediaRecipeNote = "[GIF: vision_analyze loads the first frame. Audio: use ffprobe for metadata and ffmpeg to trim/transcode; you cannot listen to audio or transcribe it. ffprobe also gives codec and timing details.]"

// DocumentsNote joins per-file notes for the current turn, appending the
// media recipe once when a GIF or audio item is present. Video needs no
// recipe: the video_analyze tool describes itself in the tool catalog.
func DocumentsNote(docs []DocumentRef) string {
	parts := make([]string, 0, len(docs))
	hasRecipe := false
	for _, doc := range docs {
		if strings.TrimSpace(doc.Path) == "" {
			continue
		}
		if doc.IsMedia() && needsRecipe(doc) {
			hasRecipe = true
		}
		parts = append(parts, DocumentContextNote(doc))
	}
	if hasRecipe {
		parts = append(parts, MediaRecipeNote)
	}
	return strings.Join(parts, "\n")
}

// needsRecipe reports whether a media item still needs the prompt recipe:
// GIFs and audio only. Video resolves through video_analyze on its own.
func needsRecipe(doc DocumentRef) bool {
	mime := normalizeMIMEType(doc.MIMEType)
	if mime == "image/gif" || strings.EqualFold(filepath.Ext(strings.TrimSpace(doc.Filename)), ".gif") {
		return true
	}
	return strings.HasPrefix(mime, "audio/")
}
