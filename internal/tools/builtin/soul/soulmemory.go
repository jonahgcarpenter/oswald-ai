package soul

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

func requestLog(log *config.Logger, ctx context.Context) *config.Logger {
	meta := requestctx.MetadataFromContext(ctx)
	principal, _ := requestctx.PrincipalFromContext(ctx)
	return log.Agent("agent.tool.soul", meta.RequestID, meta.SessionID, principal.CanonicalUserID, principal.Gateway, meta.Model)
}

// NewReadHandler returns a tool handler for the soul.read tool.
func NewReadHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		soul, err := store.Read()
		if err != nil {
			return "", fmt.Errorf("failed to read soul file: %w", err)
		}
		if soul == "" {
			return "Soul file is empty or does not exist.", nil
		}
		return soul, nil
	}
}

// NewPatchHandler returns a tool handler for the soul.patch tool.
func NewPatchHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		operation := strings.TrimSpace(strings.ToLower(stringArg(args, "operation")))
		if operation == "" {
			return "", fmt.Errorf("soul.patch: operation is required")
		}

		target := stringArg(args, "target")
		content := stringArg(args, "content")
		position := strings.TrimSpace(strings.ToLower(stringArg(args, "position")))
		anchor := stringArg(args, "anchor")

		if position == "" && operation == "add" {
			position = "end"
		}
		if err := validatePatchArgs(operation, target, content, position, anchor); err != nil {
			return "", err
		}

		reqLog := requestLog(log, ctx)
		reqLog.Info("agent.tool.soul.patched", "patched soul memory",
			config.F("tool_name", "soul.patch"),
			config.F("action", "patch"),
			config.F("operation", operation),
			config.F("position", position),
		)
		if err := store.Patch(operation, target, content, position, anchor); err != nil {
			return "", fmt.Errorf("failed to patch soul file: %w", err)
		}

		switch operation {
		case "replace":
			return "Replaced 1 line in soul file. Changes take effect on the next request.", nil
		case "remove":
			return "Removed 1 line from soul file. Changes take effect on the next request.", nil
		default:
			if position == "end" {
				return "Inserted 1 line at the end of the soul file. Changes take effect on the next request.", nil
			}
			return fmt.Sprintf("Inserted 1 line %s anchor in soul file. Changes take effect on the next request.", position), nil
		}
	}
}

func validatePatchArgs(operation, target, content, position, anchor string) error {
	switch operation {
	case "replace":
		if target == "" {
			return fmt.Errorf("soul.patch: target is required for replace")
		}
		if content == "" {
			return fmt.Errorf("soul.patch: content is required for replace")
		}
		if position != "" {
			return fmt.Errorf("soul.patch: position is not allowed for replace")
		}
		if anchor != "" {
			return fmt.Errorf("soul.patch: anchor is not allowed for replace")
		}
	case "remove":
		if target == "" {
			return fmt.Errorf("soul.patch: target is required for remove")
		}
		if content != "" {
			return fmt.Errorf("soul.patch: content is not allowed for remove")
		}
		if position != "" {
			return fmt.Errorf("soul.patch: position is not allowed for remove")
		}
		if anchor != "" {
			return fmt.Errorf("soul.patch: anchor is not allowed for remove")
		}
	case "add":
		if content == "" {
			return fmt.Errorf("soul.patch: content is required for add")
		}
		if target != "" {
			return fmt.Errorf("soul.patch: target is not allowed for add")
		}
		switch position {
		case "end":
			if anchor != "" {
				return fmt.Errorf("soul.patch: anchor is not allowed for add with position %q", position)
			}
		case "before", "after":
			if anchor == "" {
				return fmt.Errorf("soul.patch: anchor is required for add with position %q", position)
			}
		default:
			return fmt.Errorf("soul.patch: invalid add position %q", position)
		}
	default:
		return fmt.Errorf("soul.patch: invalid operation %q", operation)
	}

	if strings.Contains(target, "\n") {
		return fmt.Errorf("soul.patch: target must be a single line")
	}
	if strings.Contains(content, "\n") {
		return fmt.Errorf("soul.patch: content must be a single line")
	}
	if strings.Contains(anchor, "\n") {
		return fmt.Errorf("soul.patch: anchor must be a single line")
	}
	return nil
}

func stringArg(args map[string]interface{}, key string) string {
	value, _ := args[key].(string)
	return value
}
