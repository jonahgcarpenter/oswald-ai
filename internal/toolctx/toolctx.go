// Package toolctx provides context key helpers for passing request-scoped
// metadata (such as the sender's user ID) through the tool execution pipeline.
// It is a leaf package with no internal dependencies, so both the agent and
// individual tool packages can import it without cycles.
package toolctx

import "context"

type contextKey string

const senderIDKey contextKey = "sender_id"

// WithSenderID returns a copy of ctx with the sender's user ID attached.
// The agent calls this before executing tools so handlers can identify
// which user the request belongs to.
func WithSenderID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, senderIDKey, id)
}

// SenderIDFromContext extracts the sender's user ID from ctx.
// Returns an empty string if no ID was set.
func SenderIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(senderIDKey).(string)
	return id
}
