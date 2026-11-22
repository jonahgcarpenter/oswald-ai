// Sandboxed child environments: fixed bases plus an explicit passthrough
// allowlist resolved against the acting profile's own dotenv scope.
//
// Terminal, execute_code, and background spawns never inherit the server
// process environment. Only names registered here (skill-declared,
// config-declared, or caller-declared) reach children, and values resolve
// through the acting profile's dotenv files with an os.Getenv fallback —
// never another profile's scope. Well-known credential names can never
// register, so provider and transport secrets stay out of sandboxes even
// when a skill asks for them.
package process

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/joho/godotenv"
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// maxPassthroughNames bounds the process-wide allowlist; registration beyond
// the cap fails closed.
const maxPassthroughNames = 64

// maxScopeBytes bounds one dotenv file read for scope resolution.
const maxScopeBytes = 256 * 1024

// refusedPassthroughNames holds well-known credential and identity names that
// may never tunnel into sandboxed children, folded to uppercase because the
// Windows environment block is case-insensitive. Operator-chosen secret
// variables stay out by default-deny (only registered names pass); this list
// is the backstop against skills or config requesting provider, transport,
// relay, or runtime-identity names.
var refusedPassthroughNames = map[string]bool{
	// Provider credentials.
	"OPENAI_API_KEY": true, "ANTHROPIC_API_KEY": true, "BRAVE_API_KEY": true,
	"GITHUB_TOKEN": true, "GH_TOKEN": true,
	// Transport and gateway identities.
	"DISCORD_TOKEN": true, "TELEGRAM_BOT_TOKEN": true, "SLACK_BOT_TOKEN": true,
	"SLACK_APP_TOKEN": true, "SLACK_SIGNING_SECRET": true,
	// Relay and dashboard secrets.
	"GATEWAY_RELAY_ID": true, "GATEWAY_RELAY_SECRET": true, "GATEWAY_RELAY_DELIVERY_KEY": true,
	// Runtime identity and shell control.
	"PATH": true, "HOME": true, "LANG": true, "TMPDIR": true,
	"OSWALD_HOME": true, "OSWALD_WORKSPACE": true, "OSWALD_SCRATCH": true,
	"PYTHONPATH": true, "PYTHONHOME": true, "VIRTUAL_ENV": true, "CONDA_PREFIX": true,
}

var (
	passthroughMu    sync.Mutex
	passthroughNames = map[string]bool{}
	// profileConfigured holds operator-configured passthrough names per
	// absolutized profile root, installed once per profile at startup.
	profileConfigured = map[string][]string{}
	// nodeDirOnce resolves the server's node binary directory a single time.
	// Plugin shims (`#!/usr/bin/env node`) need node on the child PATH.
	nodeDirOnce = sync.OnceValues(func() (string, error) {
		path, err := exec.LookPath("node")
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			resolved = path
		}
		if dir := filepath.Dir(resolved); dir != "" && dir != "." {
			return dir, nil
		}
		return "", fmt.Errorf("node directory is unavailable")
	})
)

// SetProfileEnvPassthrough replaces the operator-configured allowlist for one
// profile root. Registry names always apply on top.
func SetProfileEnvPassthrough(profileRoot string, names []string) {
	abs := strings.TrimSpace(profileRoot)
	if abs != "" {
		if resolved, err := filepath.Abs(abs); err == nil {
			abs = resolved
		}
	}
	cleaned := make([]string, 0, len(names))
	for _, raw := range names {
		if name := strings.TrimSpace(raw); name != "" && envNameRE.MatchString(name) {
			cleaned = append(cleaned, name)
		}
	}
	passthroughMu.Lock()
	defer passthroughMu.Unlock()
	if abs == "" {
		return
	}
	profileConfigured[abs] = cleaned
}

// sandboxConfigured returns the operator-configured names for a profile root.
func sandboxConfigured(profileRoot string) []string {
	abs := strings.TrimSpace(profileRoot)
	if abs != "" {
		if resolved, err := filepath.Abs(abs); err == nil {
			abs = resolved
		}
	}
	passthroughMu.Lock()
	defer passthroughMu.Unlock()
	return append([]string(nil), profileConfigured[abs]...)
}

// RegisterEnvPassthrough records variable names allowed into sandboxed
// children. Refused credential names are skipped and returned so callers can
// report them; accepted names are stored verbatim (case-sensitive).
func RegisterEnvPassthrough(names []string) (refused []string) {
	passthroughMu.Lock()
	defer passthroughMu.Unlock()
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" || !envNameRE.MatchString(name) || len(passthroughNames) >= maxPassthroughNames {
			if name != "" && envNameRE.MatchString(name) && len(passthroughNames) >= maxPassthroughNames {
				refused = append(refused, name)
			}
			continue
		}
		if refusedPassthroughNames[strings.ToUpper(name)] {
			refused = append(refused, name)
			continue
		}
		passthroughNames[name] = true
	}
	return refused
}

// PassthroughNames returns the registered allowlist in stable order.
func PassthroughNames() []string {
	passthroughMu.Lock()
	defer passthroughMu.Unlock()
	out := make([]string, 0, len(passthroughNames))
	for name := range passthroughNames {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ClearEnvPassthrough resets the registered allowlist. Tests and session
// resets use it; a later skill view re-registers on demand.
func ClearEnvPassthrough() {
	passthroughMu.Lock()
	defer passthroughMu.Unlock()
	passthroughNames = map[string]bool{}
}

// ProfileSecretScope reads the acting profile's dotenv scope: the global
// default .env overlaid with the profile's own .env. Values never leave this
// package except into a spawned child environment; callers only learn whether
// a name resolved. Missing or unreadable files yield an empty scope.
func ProfileSecretScope(profileRoot string) map[string]string {
	scope := map[string]string{}
	abs := strings.TrimSpace(profileRoot)
	if abs == "" {
		return scope
	}
	globalRoot := abs
	if filepath.Base(filepath.Dir(abs)) == "profiles" {
		globalRoot = filepath.Dir(filepath.Dir(abs))
	}
	merge := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 || len(data) > maxScopeBytes {
			return
		}
		values, err := godotenv.Unmarshal(string(data))
		if err != nil {
			return
		}
		for key, value := range values {
			if envNameRE.MatchString(key) {
				scope[key] = value
			}
		}
	}
	merge(filepath.Join(globalRoot, ".env"))
	if globalRoot != abs {
		merge(filepath.Join(abs, ".env"))
	}
	return scope
}

// ResolveEnvValue resolves one allowlisted name against a profile scope with
// an os.Getenv fallback for operator exports the dotenv files do not carry.
// Refused or empty names never resolve.
func ResolveEnvValue(name string, scope map[string]string) (string, bool) {
	if name == "" || !envNameRE.MatchString(name) || refusedPassthroughNames[strings.ToUpper(name)] {
		return "", false
	}
	if scope != nil {
		if value, ok := scope[name]; ok && value != "" {
			return value, true
		}
	}
	if value := os.Getenv(name); value != "" {
		return value, true
	}
	return "", false
}

// PluginShimDirName is the leaf name of the per-profile shim directory.
// It mirrors plugins.ShimDirName without importing the plugins package
// (plugins imports this package for sandbox construction).
const PluginShimDirName = ".bin"

// ShimDirName aliases PluginShimDirName for call-site readability.
const ShimDirName = PluginShimDirName

// SandboxPath builds the one child PATH shared by terminal, execute_code,
// background processes, and cron scripts. The order is deliberate:
//
//  1. <root>/.local/bin    user installs (curl, npm -g, pip --user)
//  2. <root>/.cargo/bin    cargo install
//  3. node dir             image node, so plugin shims run
//  4. /opt/oswald          oswald CLIs (scope fence)
//  5. /usr/bin /bin /usr/local/bin
//  6. <root>/go/bin        go install
//  7. <root>/plugins/.bin  curated plugin shims
//
// Missing directories are harmless list entries (no spawn-time stat), the
// first occurrence wins, and a host without node skips its entry.
func SandboxPath(profileRoot string) string {
	root := strings.TrimSpace(profileRoot)
	entries := make([]string, 0, 10)
	seen := make(map[string]bool, 10)
	add := func(entry string) {
		if entry == "" || seen[entry] {
			return
		}
		seen[entry] = true
		entries = append(entries, entry)
	}
	if root != "" {
		add(filepath.Join(root, ".local", "bin"))
		add(filepath.Join(root, ".cargo", "bin"))
	}
	if dir, err := nodeDirOnce(); err == nil {
		add(dir)
	}
	for _, entry := range []string{"/opt/oswald", "/usr/bin", "/bin", "/usr/local/bin"} {
		add(entry)
	}
	if root != "" {
		add(filepath.Join(root, "go", "bin"))
		add(filepath.Join(root, "plugins", ShimDirName))
	}
	return strings.Join(entries, string(os.PathListSeparator))
}

// SandboxEnvForProfile appends allowlisted variables for one profile root:
// registry names plus that profile's operator-configured names, each resolved
// through the profile scope.
func SandboxEnvForProfile(base []string, profileRoot string) []string {
	return SandboxEnv(base, profileRoot, sandboxConfigured(profileRoot))
}

// SandboxEnv appends allowlisted variables to a caller-owned fixed base:
// registry names plus caller-declared configured names, each resolved through
// the profile scope. Refused and unresolvable names are skipped silently so
// one missing variable never breaks the spawn.
func SandboxEnv(base []string, profileRoot string, configured []string) []string {
	names := map[string]bool{}
	for _, name := range PassthroughNames() {
		names[name] = true
	}
	for _, raw := range configured {
		if name := strings.TrimSpace(raw); name != "" {
			names[name] = true
		}
	}
	if len(names) == 0 {
		return base
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	scope := ProfileSecretScope(profileRoot)
	out := append([]string(nil), base...)
	for _, name := range sorted {
		if !envNameRE.MatchString(name) || refusedPassthroughNames[strings.ToUpper(name)] {
			continue
		}
		if value, ok := ResolveEnvValue(name, scope); ok {
			out = append(out, name+"="+value)
		}
	}
	return out
}

// GitHubTokenEnv returns the allowlisted GitHub CLI credential entries for
// sandboxed shell environments. Only GH_TOKEN and GITHUB_TOKEN pass through;
// every other ambient secret stays sealed. Values are never logged.
func GitHubTokenEnv() []string {
	var env []string
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}
