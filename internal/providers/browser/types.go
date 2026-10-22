package browser

import "context"

// Element is one interactive node addressable by ref (e.g. @e5).
type Element struct {
	Ref  string `json:"ref"`
	Role string `json:"role"`
	Name string `json:"name"`
}

// PendingDialog is one native JavaScript dialog blocking the page.
type PendingDialog struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

// NavigateData is the model-visible result of opening a page.
type NavigateData struct {
	URL            string          `json:"url"`
	Title          string          `json:"title"`
	Snapshot       string          `json:"snapshot"`
	ElementCount   int             `json:"element_count"`
	PendingDialogs []PendingDialog `json:"pending_dialogs,omitempty"`
	BotWarning     bool            `json:"bot_warning,omitempty"`
	// Partial reports a page that never reached readiness within the
	// navigation timeout; the snapshot covers whatever had loaded.
	Partial bool `json:"partial,omitempty"`
}

// SnapshotData is the model-visible result of reading the current page.
type SnapshotData struct {
	Snapshot       string          `json:"snapshot"`
	ElementCount   int             `json:"element_count"`
	PendingDialogs []PendingDialog `json:"pending_dialogs,omitempty"`
	Truncated      bool            `json:"truncated,omitempty"`
	Path           string          `json:"path,omitempty"`
}

// ConsoleMessage is one console log entry.
type ConsoleMessage struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
}

// JSError is one uncaught page exception.
type JSError struct {
	Message string `json:"message"`
	Source  string `json:"source,omitempty"`
}

// ConsoleData merges console output and JS errors.
type ConsoleData struct {
	Messages      []ConsoleMessage `json:"console_messages"`
	Errors        []JSError        `json:"js_errors"`
	TotalMessages int              `json:"total_messages"`
	TotalErrors   int              `json:"total_errors"`
}

// EvalData is the result of evaluating a JS expression.
type EvalData struct {
	Result   string `json:"result"`
	Withheld bool   `json:"withheld,omitempty"`
}

// ImageInfo describes one page image.
type ImageInfo struct {
	Src    string `json:"src"`
	Alt    string `json:"alt,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// ImagesData lists non-data-URL page images.
type ImagesData struct {
	Images []ImageInfo `json:"images"`
	Count  int         `json:"count"`
}

// ScreenshotData carries raw PNG bytes for the handler to persist.
type ScreenshotData struct {
	PNG []byte
}

// Backend is the profile-scoped browser session contract. Implementations
// isolate profiles: one profile never drives another profile's session.
type Backend interface {
	Navigate(ctx context.Context, profile, rawURL string) (NavigateData, error)
	Snapshot(ctx context.Context, profile string, full bool) (SnapshotData, error)
	Click(ctx context.Context, profile, ref string) (SnapshotData, error)
	Fill(ctx context.Context, profile, ref, text string) (SnapshotData, error)
	Scroll(ctx context.Context, profile, direction string) (SnapshotData, error)
	Back(ctx context.Context, profile string) (NavigateData, error)
	Press(ctx context.Context, profile, key string) (SnapshotData, error)
	Console(ctx context.Context, profile string, clear bool) (ConsoleData, error)
	Eval(ctx context.Context, profile, expression string) (EvalData, error)
	Images(ctx context.Context, profile string) (ImagesData, error)
	Screenshot(ctx context.Context, profile string, annotate bool) (ScreenshotData, error)
	RespondToDialog(ctx context.Context, profile, action, promptText, dialogID string) (SnapshotData, error)
}
