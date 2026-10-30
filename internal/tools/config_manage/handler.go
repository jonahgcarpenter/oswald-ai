package config_manage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// NewHandler returns the model-facing configuration handler bound to one
// profile. Default owns the root config.yaml and .env; named profiles own
// their subtree files and may only carry providers, model, tools, MCP
// servers, and api keys (enforced by validation, not by this handler).
func NewHandler(profileRoot, profileName string) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("config_manage: authenticated principal required")
		}
		if err := rejectUnknown(args); err != nil {
			return governance.Result{}, err
		}
		globalRoot, err := config.GlobalRootForProfile(profileRoot, profileName)
		if err != nil {
			return governance.Result{}, fmt.Errorf("config_manage: %v", err)
		}
		name := profileName
		if name == "" {
			name = "default"
		}
		action, _ := args["action"].(string)
		path, _ := args["path"].(string)
		value, _ := args["value"].(string)
		envName, _ := args["name"].(string)
		switch strings.TrimSpace(action) {
		case "show":
			return show(globalRoot, name, path)
		case "get":
			return get(globalRoot, name, path)
		case "set":
			return set(globalRoot, name, path, value)
		case "unset":
			return unset(globalRoot, name, path)
		case "diff":
			return diff(globalRoot, name)
		case "undo":
			return undo(globalRoot, name)
		case "env_show":
			return envShow(globalRoot, name)
		case "env_set":
			return envSet(globalRoot, name, envName)
		case "env_unset":
			return envUnset(globalRoot, name, envName)
		default:
			return governance.Result{}, errors.New("config_manage: action must be show, get, set, unset, diff, undo, env_show, env_set or env_unset")
		}
	}
}

func rejectUnknown(args map[string]interface{}) error {
	for key := range args {
		switch key {
		case "action", "path", "value", "name":
		default:
			return fmt.Errorf("config_manage: unknown argument %q", key)
		}
	}
	return nil
}

func show(globalRoot, profile, path string) (governance.Result, error) {
	var sub []string
	if strings.TrimSpace(path) != "" {
		var err error
		sub, err = config.SplitConfigPath(path)
		if err != nil {
			return governance.Result{}, fmt.Errorf("config_manage: %v", err)
		}
	}
	configPath, err := config.ConfigPathForProfile(globalRoot, profile)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	out, err := config.ShowConfigFile(configPath, profile, sub)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	return governance.Result{Content: out, Outcome: governance.OutcomeProductive}, nil
}

func get(globalRoot, profile, path string) (governance.Result, error) {
	sub, err := config.SplitConfigPath(path)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	configPath, err := config.ConfigPathForProfile(globalRoot, profile)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	out, err := config.ShowConfigFile(configPath, profile, sub)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	return governance.Result{Content: out, Outcome: governance.OutcomeProductive}, nil
}

func set(globalRoot, profile, path, value string) (governance.Result, error) {
	sub, err := config.SplitConfigPath(path)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	if config.SensitiveConfigPath(sub) {
		return approvalNeeded(globalRoot, profile, "set", path, value, secretLeaf(sub)), nil
	}
	if err := config.SetConfigValue(globalRoot, profile, sub, value); err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	return governance.Result{Content: fmt.Sprintf("Set %s. Valid edits go live through auto-reload (log level and admission immediately, everything else flagged restart_required).", path), Outcome: governance.OutcomeProductive}, nil
}

func unset(globalRoot, profile, path string) (governance.Result, error) {
	sub, err := config.SplitConfigPath(path)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	if config.SensitiveConfigPath(sub) {
		return approvalNeeded(globalRoot, profile, "unset", path, "", secretLeaf(sub)), nil
	}
	if err := config.UnsetConfigValue(globalRoot, profile, sub); err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	return governance.Result{Content: fmt.Sprintf("Unset %s.", path), Outcome: governance.OutcomeProductive}, nil
}

func diff(globalRoot, profile string) (governance.Result, error) {
	configPath, err := config.ConfigPathForProfile(globalRoot, profile)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	out, err := config.DiffConfigText(configPath)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	return governance.Result{Content: out, Outcome: governance.OutcomeProductive}, nil
}

func undo(globalRoot, profile string) (governance.Result, error) {
	configPath, err := config.ConfigPathForProfile(globalRoot, profile)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: configuration file unavailable")
	}
	previous, err := config.ReadLastKnownGood(configPath)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: nothing to undo")
	}
	if config.ConfigChangeSensitive(current, previous) {
		return approvalNeeded(globalRoot, profile, "undo", "", "", true), nil
	}
	if err := config.UndoConfigValue(globalRoot, profile); err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	return governance.Result{Content: "Restored last-known-good.", Outcome: governance.OutcomeProductive}, nil
}

func envShow(globalRoot, profile string) (governance.Result, error) {
	envPath, err := config.EnvPathForProfile(globalRoot, profile)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	names, err := config.ListEnvNames(envPath)
	if err != nil {
		return governance.Result{}, fmt.Errorf("config_manage: %v", err)
	}
	if len(names) == 0 {
		return governance.Result{Content: "(no variables)", Outcome: governance.OutcomeProductive}, nil
	}
	return governance.Result{Content: strings.Join(names, "\n"), Outcome: governance.OutcomeProductive}, nil
}

func envSet(globalRoot, profile, name string) (governance.Result, error) {
	if strings.TrimSpace(name) == "" {
		return governance.Result{}, errors.New("config_manage: env_set needs a variable name")
	}
	return approvalNeeded(globalRoot, profile, "env_set", "", "", true), nil
}

func envUnset(globalRoot, profile, name string) (governance.Result, error) {
	if strings.TrimSpace(name) == "" {
		return governance.Result{}, errors.New("config_manage: env_unset needs a variable name")
	}
	return approvalNeeded(globalRoot, profile, "env_unset", "", "", true), nil
}

// secretLeaf reports whether a path carries credential material, in which
// case the approval hint must not echo the proposed value.
func secretLeaf(sub []string) bool {
	// Any non-inline secret leaf is secret; inline paths are never secret.
	for _, segment := range sub {
		lower := strings.ToLower(segment)
		switch lower {
		case "headers", "env", "oauth", "key", "brave_key", "token", "server_password", "client_secret", "client_id", "password", "secret", "authorization":
			return true
		}
	}
	return false
}

// approvalNeeded is the productive denial returned when a write needs the
// operator: nothing is applied, and the model gets the exact CLI command for
// the user. Secret values are never echoed.
func approvalNeeded(globalRoot, profile, op, path, value string, secret bool) governance.Result {
	profileFlag := ""
	if profile != "" && profile != "default" {
		profileFlag = " --profile " + profile
	}
	var command string
	switch op {
	case "set":
		if secret {
			command = fmt.Sprintf("oswald config set %s <value>%s", path, profileFlag)
		} else {
			command = fmt.Sprintf("oswald config set %s %s%s", path, quoteCLIValue(value), profileFlag)
		}
	case "unset":
		command = fmt.Sprintf("oswald config unset %s%s", path, profileFlag)
	case "undo":
		command = fmt.Sprintf("oswald config undo%s", profileFlag)
	case "env_set":
		command = fmt.Sprintf("oswald env set <NAME> <value>%s", profileFlag)
	case "env_unset":
		command = fmt.Sprintf("oswald env unset <NAME>%s", profileFlag)
	}
	_ = globalRoot
	return governance.Result{Content: fmt.Sprintf("This change needs operator approval and was not applied. Tell the user to run: %s — valid edits go live through auto-reload. Never print secret values in chat.", command), Outcome: governance.OutcomeProductive}
}

func quoteCLIValue(value string) string {
	if strings.ContainsAny(value, " \t\n\"'") || strings.TrimSpace(value) == "" {
		return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
	}
	return value
}
