package requestctx

import (
	"context"
	"strings"
)

const parentModelKey contextKey = "parent_model"

// ParentModel carries the calling turn's resolved model routing for delegated
// children. Strings only, so this package never imports providers/llm.
type ParentModel struct {
	Model    string
	Provider string
}

// WithParentModel attaches the calling turn's resolved routing. Empty values
// attach nothing (treated as absent downstream).
func WithParentModel(ctx context.Context, model, provider string) context.Context {
	model = strings.TrimSpace(model)
	provider = strings.TrimSpace(provider)
	if model == "" || provider == "" {
		return ctx
	}
	return context.WithValue(ctx, parentModelKey, ParentModel{Model: model, Provider: provider})
}

// ParentModelFromContext extracts the calling turn's routing.
func ParentModelFromContext(ctx context.Context) (ParentModel, bool) {
	parent, ok := ctx.Value(parentModelKey).(ParentModel)
	return parent, ok
}
