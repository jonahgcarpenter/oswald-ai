// Package requestctx carries authenticated profile ownership and request-local
// metadata through the agent, provider, and tool pipeline.
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
	inputImagesKey contextKey = "input_images"
)

// ToolExposer records request-local tool discovery, not lasting authorization.
type ToolExposer interface{ ExposeTools([]string) }

// Metadata carries trusted request correlation and current-turn context.
type Metadata struct {
	RequestID, SessionID                      string
	SessionGeneration                         int
	Model, CurrentUserText                    string
	GroupGateway, GroupChatID, PublicUserText string
	Workload, OperationID, ParentOperationID  string
	JobID                                     int64
}

// InputImage is a normalized current or generated request reference.
type InputImage struct {
	ID, Path, ImageID      string
	Version                int
	ParentSourceImageID    string
	VersionHighwater       int
	MIMEType, Data, Source string
}

// WithPrincipal attaches the trusted profile owner to ctx.
func WithPrincipal(ctx context.Context, p identity.Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFromContext extracts the authenticated request actor.
func PrincipalFromContext(ctx context.Context) (identity.Principal, bool) {
	p, ok := ctx.Value(principalKey).(identity.Principal)
	return p, ok
}

// WithMetadata attaches request correlation without global mutable state.
func WithMetadata(ctx context.Context, meta Metadata) context.Context {
	return context.WithValue(ctx, requestMetaKey, meta)
}

// MetadataFromContext extracts request-local metadata.
func MetadataFromContext(ctx context.Context) Metadata {
	meta, _ := ctx.Value(requestMetaKey).(Metadata)
	return meta
}

// WithToolExposer attaches the request's discovery collector.
func WithToolExposer(ctx context.Context, e ToolExposer) context.Context {
	return context.WithValue(ctx, toolExposeKey, e)
}

// ToolExposerFromContext extracts the discovery collector.
func ToolExposerFromContext(ctx context.Context) ToolExposer {
	e, _ := ctx.Value(toolExposeKey).(ToolExposer)
	return e
}

// WithInputImages attaches a defensive copy of the image catalog.
func WithInputImages(ctx context.Context, images []InputImage) context.Context {
	return context.WithValue(ctx, inputImagesKey, append([]InputImage(nil), images...))
}

// InputImagesFromContext returns a defensive copy of the current catalog.
func InputImagesFromContext(ctx context.Context) []InputImage {
	images, _ := ctx.Value(inputImagesKey).([]InputImage)
	return append([]InputImage(nil), images...)
}
