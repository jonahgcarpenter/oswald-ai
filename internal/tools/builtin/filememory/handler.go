// Package filememory adapts authenticated model calls to private file notes.
package filememory

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// NewHandler returns the model-facing private file memory mutation handler.
func NewHandler(store *files.Store) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("memory: authenticated principal required")
		}
		if store == nil {
			return governance.Result{}, errors.New("memory: store unavailable")
		}
		target, ok := args["target"].(string)
		if !ok || (target != "user" && target != "memory") {
			return governance.Result{}, errors.New("memory: target must be user or memory")
		}
		var ops []files.Operation
		if raw, exists := args["operations"]; exists {
			if len(args) != 2 {
				return governance.Result{}, errors.New("memory: operations cannot be combined with single-operation fields")
			}
			items, ok := raw.([]interface{})
			if !ok || len(items) == 0 || len(items) > 20 {
				return governance.Result{}, errors.New("memory: invalid operations")
			}
			for _, value := range items {
				item, ok := value.(map[string]interface{})
				if !ok {
					return governance.Result{}, errors.New("memory: invalid operation")
				}
				op, err := decodeOperation(item)
				if err != nil {
					return governance.Result{}, err
				}
				ops = append(ops, op)
			}
		} else {
			item := make(map[string]interface{}, len(args)-1)
			for key, value := range args {
				if key != "target" {
					item[key] = value
				}
			}
			op, err := decodeOperation(item)
			if err != nil {
				return governance.Result{}, err
			}
			ops = []files.Operation{op}
		}
		content, err := store.Apply(ctx, principal.CanonicalUserID, target, ops)
		if err != nil {
			return governance.Result{}, fmt.Errorf("memory: %w", err)
		}
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func decodeOperation(item map[string]interface{}) (files.Operation, error) {
	var op files.Operation
	for key := range item {
		if key != "action" && key != "content" && key != "new_text" && key != "old_text" {
			return op, errors.New("memory: unknown operation field")
		}
	}
	action, ok := item["action"].(string)
	if !ok {
		return op, errors.New("memory: invalid action")
	}
	op.Action = action
	for key, raw := range item {
		if key == "action" {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return op, errors.New("memory: operation text must be a string")
		}
		switch key {
		case "content":
			op.Content = value
		case "new_text":
			op.Content = value
		case "old_text":
			op.OldText = value
		}
	}
	_, hasContent := item["content"]
	_, hasNewText := item["new_text"]
	if hasContent && hasNewText {
		return op, errors.New("memory: content and new_text conflict")
	}
	switch action {
	case "add":
		if !hasContent || hasNewText || op.OldText != "" {
			return op, errors.New("memory: add requires content only")
		}
	case "replace":
		if (!hasContent && !hasNewText) || op.OldText == "" {
			return op, errors.New("memory: replace requires old_text and content or new_text")
		}
	case "remove":
		if hasContent || hasNewText || op.OldText == "" {
			return op, errors.New("memory: remove requires old_text only")
		}
	default:
		return op, errors.New("memory: action must be add, replace or remove")
	}
	return op, nil
}
