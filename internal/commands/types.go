package commands

import (
	"context"
	"fmt"
	"mime"
	"strings"
	"unicode"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/privacyruntime"
)

const (
	// MaxAttachmentBytes is the maximum size of one command attachment.
	MaxAttachmentBytes = 8 << 20
	// MaxAttachments is the maximum number of attachments in one command response.
	MaxAttachments = 10
	// MaxTotalAttachmentBytes is the maximum combined attachment size in one command response.
	MaxTotalAttachmentBytes = MaxAttachments * MaxAttachmentBytes
)

// Definition describes a command registered with the command service.
type Definition struct {
	Name          string
	Aliases       []string
	Summary       string
	Usage         string
	AdminOnly     bool
	UserExclusive bool
}

// Request is the gateway-neutral command execution context.
type Request struct {
	RequestID   string
	Principal   identity.Principal
	ChatID      string
	SessionKey  string
	DisplayName string
	ClientID    string
	IsDirect    bool
	IsGroup     bool

	Raw      string
	Name     string
	Args     []string
	ArgsText string
}

// Attachment is an in-memory file delivered with a command response.
type Attachment struct {
	Filename string
	MIMEType string
	Data     []byte
}

// Validate checks that the attachment is safe for transport delivery.
func (a Attachment) Validate() error {
	filename := strings.TrimSpace(a.Filename)
	if filename == "" {
		return fmt.Errorf("command attachment filename is required")
	}
	if filename != a.Filename {
		return fmt.Errorf("command attachment filename has leading or trailing whitespace")
	}
	if filename == "." || filename == ".." || strings.ContainsAny(filename, `/\\`) {
		return fmt.Errorf("command attachment filename must be a base name")
	}
	if len(filename) > 255 {
		return fmt.Errorf("command attachment filename exceeds 255 bytes")
	}
	for _, r := range filename {
		if unicode.IsControl(r) {
			return fmt.Errorf("command attachment filename contains control characters")
		}
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(a.MIMEType))
	if err != nil || mediaType == "" || !strings.Contains(mediaType, "/") {
		return fmt.Errorf("command attachment MIME type is invalid")
	}
	if len(a.Data) == 0 {
		return fmt.Errorf("command attachment data is required")
	}
	if len(a.Data) > MaxAttachmentBytes {
		return fmt.Errorf("command attachment exceeds %d bytes", MaxAttachmentBytes)
	}
	return nil
}

// Result is the user-facing command response.
type Result struct {
	Text         string
	Attachment   *Attachment
	Attachments  []Attachment
	Invalidation *privacyruntime.Event `json:"-"`
}

// OrderedAttachments returns the attachments in delivery order. Attachments is
// canonical when populated; Attachment preserves compatibility with callers
// that return one file.
func (r Result) OrderedAttachments() []Attachment {
	if len(r.Attachments) > 0 {
		return r.Attachments
	}
	if r.Attachment != nil {
		return []Attachment{*r.Attachment}
	}
	return nil
}

// ValidateAttachments validates per-file and aggregate transport limits.
func (r Result) ValidateAttachments() error {
	attachments := r.OrderedAttachments()
	if len(attachments) > MaxAttachments {
		return fmt.Errorf("command response exceeds %d attachments", MaxAttachments)
	}
	filenames := make(map[string]struct{}, len(attachments))
	totalBytes := 0
	for i, attachment := range attachments {
		if err := attachment.Validate(); err != nil {
			return fmt.Errorf("command attachment %d: %w", i+1, err)
		}
		if _, exists := filenames[attachment.Filename]; exists {
			return fmt.Errorf("command response contains duplicate filename %q", attachment.Filename)
		}
		filenames[attachment.Filename] = struct{}{}
		totalBytes += len(attachment.Data)
		if totalBytes > MaxTotalAttachmentBytes {
			return fmt.Errorf("command response attachments exceed %d total bytes", MaxTotalAttachmentBytes)
		}
	}
	return nil
}

// UsageText renders the standard command usage response.
func UsageText(definition Definition) string {
	if definition.Summary == "" {
		return "Use: " + definition.Usage
	}
	return definition.Summary + "\nUse: " + definition.Usage
}

// Handler executes one registered command.
type Handler interface {
	Definition() Definition
	Execute(context.Context, Request) (Result, error)
}

// FenceTargetResolver resolves canonical users whose normal work must be
// excluded while a command executes. The service passes a parsed Request.
type FenceTargetResolver interface {
	ResolveFenceTargets(context.Context, Request) ([]string, error)
}

// HandlerFunc adapts a function to a command handler.
type HandlerFunc struct {
	DefinitionValue         Definition
	ExecuteFunc             func(context.Context, Request) (Result, error)
	ResolveFenceTargetsFunc func(context.Context, Request) ([]string, error)
}

// Definition returns the function handler's command metadata.
func (h HandlerFunc) Definition() Definition {
	return h.DefinitionValue
}

// Execute runs the wrapped function.
func (h HandlerFunc) Execute(ctx context.Context, req Request) (Result, error) {
	return h.ExecuteFunc(ctx, req)
}

// ResolveFenceTargets resolves optional command-specific privacy fences.
func (h HandlerFunc) ResolveFenceTargets(ctx context.Context, req Request) ([]string, error) {
	if h.ResolveFenceTargetsFunc == nil {
		return nil, nil
	}
	return h.ResolveFenceTargetsFunc(ctx, req)
}

// Middleware wraps a command handler with cross-cutting behavior.
type Middleware func(Handler) Handler

// Command registers a handler and its middleware with the command service.
type Command struct {
	Handler    Handler
	Middleware []Middleware
}

// Authorizer checks command-level permissions for canonical users.
type Authorizer interface {
	IsAdmin(canonicalUserID string) (bool, error)
}
