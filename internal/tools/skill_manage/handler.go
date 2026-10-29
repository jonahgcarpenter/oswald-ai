package skill_manage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/skills"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	outputpkg "github.com/jonahgcarpenter/oswald-ai/internal/tools/output"
)

// NewHandler returns the model-facing skill handler bound to one profile's
// managed skills tree. It bypasses workspace file groups (which deny
// direct skills/ writes) the same way the memory tool bypasses them for
// memories/: the tool is the mediated path.
func NewHandler(profileRoot string) func(context.Context, map[string]interface{}) (governance.Result, error) {
	managed := ""
	if strings.TrimSpace(profileRoot) != "" {
		managed = filepath.Join(profileRoot, "skills")
	}
	store := skills.NewStore(managed)
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("skill_manage: authenticated principal required")
		}
		if err := rejectUnknown(args); err != nil {
			return governance.Result{}, err
		}
		action, _ := args["action"].(string)
		platform := platformOf(principal.Gateway)
		switch strings.TrimSpace(action) {
		case "list":
			return list(store, platform)
		case "view":
			name, _ := args["name"].(string)
			filePath, _ := args["file_path"].(string)
			return view(store, name, filePath, platform)
		case "create":
			name, _ := args["name"].(string)
			description, _ := args["description"].(string)
			content, _ := args["content"].(string)
			return create(store, name, description, content)
		case "update":
			name, _ := args["name"].(string)
			oldText, _ := args["old_text"].(string)
			content, _ := args["content"].(string)
			return update(store, name, oldText, content)
		default:
			return governance.Result{}, errors.New("skill_manage: action must be list, view, create or update")
		}
	}
}

// platformOf normalizes the gateway name to the platform scope skills
// gate on. Unknown transports match only ungated skills.
func platformOf(gateway string) string {
	return strings.ToLower(strings.TrimSpace(gateway))
}

func rejectUnknown(args map[string]interface{}) error {
	for key := range args {
		switch key {
		case "action", "name", "file_path", "description", "content", "old_text":
		default:
			return fmt.Errorf("skill_manage: unknown argument %q", key)
		}
	}
	return nil
}

func list(store *skills.Store, platform string) (governance.Result, error) {
	found := store.List(platform)
	if len(found) == 0 {
		return governance.Result{Content: "(no skills)", Outcome: governance.OutcomeUnproductive}, nil
	}
	const maxRows = 200
	var sb strings.Builder
	for i, skill := range found {
		if i >= maxRows {
			fmt.Fprintf(&sb, "... (%d more; refine by category)\n", len(found)-maxRows)
			break
		}
		fmt.Fprintf(&sb, "%s [%s]: %s\n", skill.LoadName, skill.Category, outputpkg.Clean(skill.Description))
	}
	return governance.Result{Content: strings.TrimSuffix(sb.String(), "\n") + "\nLoad a skill with skill_manage(action=\"view\", name=\"<skill>\").", Outcome: governance.OutcomeProductive}, nil
}

func view(store *skills.Store, name, filePath, platform string) (governance.Result, error) {
	result, err := store.View(name, filePath, platform)
	if err != nil {
		return governance.Result{}, fmt.Errorf("skill_manage: %w", err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Skill: %s [%s]\n", result.LoadName, result.Category)
	if result.Version != "" {
		fmt.Fprintf(&sb, "Version: %s\n", outputpkg.Clean(result.Version))
	}
	if len(result.Prerequisites) > 0 {
		fmt.Fprintf(&sb, "Prerequisites: %s\n", outputpkg.Clean(strings.Join(result.Prerequisites, ", ")))
	}
	sb.WriteString("\n")
	if result.FilePath != "" {
		sb.WriteString(outputpkg.Clean(result.FileContent))
		return governance.Result{Content: sb.String(), Outcome: governance.OutcomeProductive}, nil
	}
	sb.WriteString(outputpkg.Clean(result.Body))
	if len(result.Supporting) > 0 {
		sb.WriteString("\n\nSupporting files (read with file_path):\n")
		for _, file := range result.Supporting {
			sb.WriteString("- " + file + "\n")
		}
	}
	return governance.Result{Content: strings.TrimSuffix(sb.String(), "\n"), Outcome: governance.OutcomeProductive}, nil
}

func create(store *skills.Store, name, description, content string) (governance.Result, error) {
	skill, err := store.Create(name, description, content)
	if err != nil {
		return governance.Result{}, fmt.Errorf("skill_manage: %w", err)
	}
	return governance.Result{Content: fmt.Sprintf("Skill %q created. It now appears in the skills index for future turns.", skill.LoadName), Outcome: governance.OutcomeProductive}, nil
}

func update(store *skills.Store, name, oldText, content string) (governance.Result, error) {
	skill, err := store.Update(name, oldText, content)
	if err != nil {
		return governance.Result{}, fmt.Errorf("skill_manage: %w", err)
	}
	return governance.Result{Content: fmt.Sprintf("Skill %q updated.", skill.LoadName), Outcome: governance.OutcomeProductive}, nil
}
