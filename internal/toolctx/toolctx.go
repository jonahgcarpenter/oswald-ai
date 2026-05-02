// Package toolctx provides context key helpers for passing request-scoped
// metadata through the tool execution pipeline.
package toolctx

import "context"

type contextKey string

const (
	requestMetaKey contextKey = "request_meta"
	senderIDKey    contextKey = "sender_id"
)

// Metadata carries request-scoped fields needed by tools and provider logging.
type Metadata struct {
	RequestID string
	SessionID string
	SenderID  string
	Gateway   string
	Model     string
}

// WithSenderID returns a copy of ctx with the sender's user ID attached.
func WithSenderID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, senderIDKey, id)
}

// SenderIDFromContext extracts the sender's user ID from ctx.
func SenderIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(senderIDKey).(string)
	return id
}

// WithMetadata returns a copy of ctx with request metadata attached.
func WithMetadata(ctx context.Context, meta Metadata) context.Context {
	return context.WithValue(ctx, requestMetaKey, meta)
}

// MetadataFromContext extracts request metadata from ctx.
func MetadataFromContext(ctx context.Context) Metadata {
	meta, _ := ctx.Value(requestMetaKey).(Metadata)
	if meta.SenderID == "" {
		meta.SenderID = SenderIDFromContext(ctx)
	}
	return meta
}
