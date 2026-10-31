package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OswaldHomeEnv carries the calling profile's root into operator CLI
// subprocesses (terminal, background processes, code execution, cron
// scripts). It names the profile root itself: the global root for the
// default profile, <global>/profiles/<name>/ for named profiles. Bare
// `oswald config|env ...` invocations resolve their scope from it so the
// agent's edits land on the chatting profile instead of silently defaulting.
const OswaldHomeEnv = "OSWALD_HOME"

// InferScopeFromHome derives the global configuration root and owning
// profile from an OSWALD_HOME value. Anything that is not an existing
// directory is ignored (ok=false) so a stray environment never redirects
// operator commands.
func InferScopeFromHome(home string) (globalRoot, profile string, ok bool) {
	trimmed := strings.TrimSpace(home)
	if trimmed == "" {
		return "", "", false
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", "", false
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", "", false
	}
	abs = filepath.Clean(abs)
	if filepath.Base(filepath.Dir(abs)) == "profiles" && ValidProfileName(filepath.Base(abs)) {
		return filepath.Dir(filepath.Dir(abs)), filepath.Base(abs), true
	}
	return abs, "default", true
}

// ResolveGlobalRoot resolves the configuration root for an operator CLI
// invocation. An explicit --root flag wins; otherwise a valid OSWALD_HOME
// scopes to its global root; otherwise the compiled default applies. An
// explicit flag contradicting a valid OSWALD_HOME fails closed so an agent
// cannot silently retarget another install.
func ResolveGlobalRoot(flagRoot string) (string, error) {
	if strings.TrimSpace(flagRoot) != "" {
		abs, err := filepath.Abs(flagRoot)
		if err != nil {
			return "", fmt.Errorf("invalid configuration root")
		}
		if global, _, ok := InferScopeFromHome(os.Getenv(OswaldHomeEnv)); ok {
			if filepath.Clean(abs) != filepath.Clean(global) {
				return "", fmt.Errorf("oswald: --root does not match current profile scope")
			}
		}
		return flagRoot, nil
	}
	if global, _, ok := InferScopeFromHome(os.Getenv(OswaldHomeEnv)); ok {
		return global, nil
	}
	return OswaldHomeDir, nil
}

// ResolveProfile resolves the operator profile for an `oswald config|env`
// invocation. An explicit --profile flag wins; otherwise a valid
// OSWALD_HOME supplies the chatting profile; otherwise default applies. An
// explicit flag contradicting a valid OSWALD_HOME fails closed so an agent
// cannot silently edit another profile's files.
func ResolveProfile(flagProfile string) (string, error) {
	if strings.TrimSpace(flagProfile) != "" {
		name := strings.TrimSpace(flagProfile)
		if name != "default" && !ValidProfileName(name) {
			return "", fmt.Errorf("invalid profile name %q", flagProfile)
		}
		if _, inferred, ok := InferScopeFromHome(os.Getenv(OswaldHomeEnv)); ok {
			if name != inferred {
				return "", fmt.Errorf("oswald: --profile %q does not match current profile %q", name, inferred)
			}
		}
		return name, nil
	}
	if _, inferred, ok := InferScopeFromHome(os.Getenv(OswaldHomeEnv)); ok {
		return inferred, nil
	}
	return "default", nil
}
