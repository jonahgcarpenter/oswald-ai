// Package requestctx provides context key helpers for passing request-scoped
// metadata through the tool execution pipeline.
package requestctx

import (
	"context"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

type contextKey string

const (
	requestMetaKey contextKey = "request_meta"
	principalKey   contextKey = "principal"
	toolExposeKey  contextKey = "tool_exposer"
)

// GlobalToolEvidence is one successful global MCP result available only during
// the request that executed it.
type GlobalToolEvidence struct {
	ToolCallID     string
	ServerID       string
	ServerName     string
	ToolName       string
	RemoteToolName string
	ArgumentsJSON  string
	Result         string
}

// ToolExposer records tools that should become visible for the active request.
type ToolExposer interface {
	ExposeTools(names []string)
	RecordGlobalToolEvidence(evidence GlobalToolEvidence)
	GlobalToolEvidence(toolCallID string) (GlobalToolEvidence, bool)
}

// Metadata carries request-scoped fields needed by tools and provider logging.
type Metadata struct {
	RequestID         string
	SessionID         string
	SessionGeneration int
	Model             string
	CurrentUserText   string
}

// WithPrincipal returns a copy of ctx with the resolved request actor attached.
func WithPrincipal(ctx context.Context, principal identity.Principal) context.Context {
	return context.WithValue(ctx, principalKey, principal)
}

// PrincipalFromContext extracts the resolved request actor from ctx.
func PrincipalFromContext(ctx context.Context) (identity.Principal, bool) {
	principal, ok := ctx.Value(principalKey).(identity.Principal)
	return principal, ok
}

// WithMetadata returns a copy of ctx with request metadata attached.
func WithMetadata(ctx context.Context, meta Metadata) context.Context {
	return context.WithValue(ctx, requestMetaKey, meta)
}

// MetadataFromContext extracts request metadata from ctx.
func MetadataFromContext(ctx context.Context) Metadata {
	meta, _ := ctx.Value(requestMetaKey).(Metadata)
	return meta
}

// WithToolExposer returns a copy of ctx with the active request's tool exposer attached.
func WithToolExposer(ctx context.Context, exposer ToolExposer) context.Context {
	return context.WithValue(ctx, toolExposeKey, exposer)
}

// ToolExposerFromContext extracts the active request's tool exposer from ctx.
func ToolExposerFromContext(ctx context.Context) ToolExposer {
	exposer, _ := ctx.Value(toolExposeKey).(ToolExposer)
	return exposer
}
