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
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

// NewHandler returns the model-facing skill handler bound to one profile's
// managed skills tree. It bypasses workspace file groups (which deny
// direct skills/ writes) the same way the memory tool bypasses them for
// memories/: the tool is the mediated path.
func NewHandler(profileRoot string) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return NewHandlerWithPlugins(profileRoot, nil)
}

// NewHandlerWithPlugins binds the managed tree plus read-only bundled plugin
// skills roots. Plugin skills load under "<key>/<load name>" and reject
// create/update; view works unchanged, including required_env registration.
func NewHandlerWithPlugins(profileRoot string, pluginSkills []skills.PluginRoot) func(context.Context, map[string]interface{}) (governance.Result, error) {
	managed := ""
	if strings.TrimSpace(profileRoot) != "" {
		managed = filepath.Join(profileRoot, "skills")
	}
	store := skills.NewStore(managed)
	for _, root := range pluginSkills {
		store.BindPlugin(root.Key, root.Dir)
	}
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
			return view(store, profileRoot, pluginSkills, name, filePath, platform)
		case "create":
			name, _ := args["name"].(string)
			description, _ := args["description"].(string)
			content, _ := args["content"].(string)
			category, _ := args["category"].(string)
			return create(store, name, description, content, category)
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
		case "action", "name", "file_path", "description", "content", "old_text", "category":
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

func view(store *skills.Store, profileRoot string, pluginSkills []skills.PluginRoot, name, filePath, platform string) (governance.Result, error) {
	result, err := store.View(name, filePath, platform)
	if err != nil {
		return governance.Result{}, fmt.Errorf("skill_manage: %w", err)
	}
	// A viewed skill's declared sandbox variables register for passthrough so
	// later terminal and execute_code calls in this process can receive them
	// from the profile scope. Provider credential names are refused.
	refusedNote := ""
	if refused := process.RegisterEnvPassthrough(result.RequiredEnv); len(refused) > 0 {
		refusedNote = fmt.Sprintf("\n\nnote: %d requested variable(s) were refused.", len(refused))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Skill: %s [%s]\n", result.LoadName, result.Category)
	if result.Version != "" {
		fmt.Fprintf(&sb, "Version: %s\n", outputpkg.Clean(result.Version))
	}
	sb.WriteString("\n")
	if result.FilePath != "" {
		sb.WriteString(outputpkg.Clean(result.FileContent))
		return governance.Result{Content: sb.String() + refusedNote, Outcome: governance.OutcomeProductive}, nil
	}
	sb.WriteString(outputpkg.Clean(result.Body))
	if len(result.Supporting) > 0 {
		sb.WriteString("\n\nSupporting files (read with file_path):\n")
		for _, file := range result.Supporting {
			sb.WriteString("- " + file + "\n")
		}
	}
	sb.WriteString(pluginRuntimeNote(profileRoot, pluginSkills, result.Source))
	return governance.Result{Content: strings.TrimSuffix(sb.String(), "\n") + refusedNote, Outcome: governance.OutcomeProductive}, nil
}

// pluginRuntimeNote orients the model for a bundled plugin skill view: where
// the sidecar binary lives, which HOME its commands run under, and that repo
// install guides written for other agent systems do not apply here. Profile
// skills (empty source) and supporting-file reads get no note.
func pluginRuntimeNote(profileRoot string, pluginSkills []skills.PluginRoot, source string) string {
	if strings.TrimSpace(source) == "" {
		return ""
	}
	var skillsDir string
	for _, root := range pluginSkills {
		if root.Key == source {
			skillsDir = root.Dir
			break
		}
	}
	if strings.TrimSpace(skillsDir) == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nPlugin runtime: this skill ships with plugin \"" + source + "\".")
	sb.WriteString(" Its CLI lives under " + filepath.Join(filepath.Dir(skillsDir), "node_modules", ".bin") + ";")
	sb.WriteString(" add that directory to PATH for terminal calls instead of installing anything.")
	if strings.TrimSpace(profileRoot) != "" {
		sb.WriteString(" Run its commands with HOME set to " + filepath.Join(profileRoot, "plugins", filepath.FromSlash(source), "home") + ";")
		sb.WriteString(" credentials live in that home's config.")
	}
	sb.WriteString(" Setup guides in the repo written for other agent systems do not apply here.")
	return sb.String()
}

func create(store *skills.Store, name, description, content, category string) (governance.Result, error) {
	skill, err := store.Create(name, description, content, category)
	if err != nil {
		return governance.Result{}, fmt.Errorf("skill_manage: %w", err)
	}
	return governance.Result{Content: fmt.Sprintf("Skill %q created under category %q. It now appears in the skills index for future turns.", skill.LoadName, skill.Category), Outcome: governance.OutcomeProductive}, nil
}

func update(store *skills.Store, name, oldText, content string) (governance.Result, error) {
	skill, err := store.Update(name, oldText, content)
	if err != nil {
		return governance.Result{}, fmt.Errorf("skill_manage: %w", err)
	}
	return governance.Result{Content: fmt.Sprintf("Skill %q updated.", skill.LoadName), Outcome: governance.OutcomeProductive}, nil
}
