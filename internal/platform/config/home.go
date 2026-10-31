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

// ResolveReadProfile resolves the operator profile for read-only CLI
// invocations (`show`, `get`, `list`, `validate`, `test`). Unlike
// ResolveProfile it permits naming another profile: reads are visible
// cross-profile while every mutation stays fenced to the calling scope.
// Malformed names fail here; unknown names fail closed downstream when the
// profile file is located.
func ResolveReadProfile(flagProfile string) (string, error) {
	if strings.TrimSpace(flagProfile) != "" {
		name := strings.TrimSpace(flagProfile)
		if name != "default" && !ValidProfileName(name) {
			return "", fmt.Errorf("invalid profile name %q", flagProfile)
		}
		return name, nil
	}
	if _, inferred, ok := InferScopeFromHome(os.Getenv(OswaldHomeEnv)); ok {
		return inferred, nil
	}
	return "default", nil
}

// RequireDefaultScope permits mutations of global configuration (the
// gateway, platforms, and runtime sections) and profile lifecycle
// operations. A valid OSWALD_HOME outside the default profile denies them:
// a named profile's agent must not reshape shared state. A local operator
// invocation without OSWALD_HOME is unaffected.
func RequireDefaultScope() error {
	if _, inferred, ok := InferScopeFromHome(os.Getenv(OswaldHomeEnv)); ok {
		if inferred != "default" {
			return fmt.Errorf("oswald: global configuration can only be changed from the default profile scope (current profile %q)", inferred)
		}
	}
	return nil
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
