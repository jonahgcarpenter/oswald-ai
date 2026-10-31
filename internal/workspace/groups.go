package workspace

import (
	"errors"
	"path/filepath"
	"strings"
)

// Same-trust file groups. Profiles within one install share trust — true
// isolation means a separate container. Secrets and operator configuration
// (config.yaml, .env) stay sealed everywhere: inspect config.yaml through
// `oswald config show|get` and edit it through the scoped config groups
// (`oswald providers|model|api|gateway|platforms|tools|runtime|mcp`);
// .env goes through `oswald env`. Sessions stay fenced
// by state.db; everything else is readable, with writes gated by group.
type Group int

const (
	// GroupProtected holds credentials and secrets: no read, no write.
	GroupProtected Group = iota
	// GroupSystem is tool-managed state: read allowed, writes denied.
	GroupSystem
	// GroupAgent is agent-owned: read and write allowed.
	GroupAgent
)

// DenyError marks a path refused by the workspace file groups. Error stays
// generic for logs; handlers render DenyMessage for the model instead.
type DenyError struct {
	Rel   string
	Group Group
}

func (e *DenyError) Error() string { return "path is not accessible" }

// AsDeny extracts a group denial from a Resolve/ResolveWrite error.
func AsDeny(err error) (string, Group, bool) {
	var derr *DenyError
	if errors.As(err, &derr) {
		return derr.Rel, derr.Group, true
	}
	return "", GroupAgent, false
}

// DenyMessage renders the group message for a read (write=false) or write
// (write=true) refusal. Reads are only ever denied for protected paths, so
// a system group here always pairs with write=true.
func DenyMessage(rel string, group Group, write bool) string {
	switch group {
	case GroupProtected:
		if !write && isStateDBBase(filepath.Base(rel)) {
			return "Access denied: " + rel + " is application-owned state and cannot be read directly; use session_search for conversation history."
		}
		if filepath.Base(rel) == "config.yaml" {
			if write {
				return "Write denied: '" + rel + "' is protected and cannot be written directly; edit via the scoped terminal groups (`oswald providers|model|api|gateway|platforms|tools|runtime|mcp`), never by hand."
			}
			return "Access denied: '" + rel + "' is protected and cannot be read directly; use `oswald config show|get` via terminal."
		}
		if base := filepath.Base(rel); base == ".env" || strings.HasPrefix(base, ".env.") {
			if write {
				return "Write denied: '" + rel + "' is protected and cannot be written directly; use `oswald env set|unset` via terminal."
			}
			return "Access denied: '" + rel + "' is protected and cannot be accessed directly; use `oswald env show` via terminal (values are never printed)."
		}
		if write {
			return "Write denied: '" + rel + "' is protected and cannot be written."
		}
		return "Access denied: '" + rel + "' is protected and cannot be accessed."
	case GroupSystem:
		if write {
			if hasPathComponent(rel, "memories") {
				return "Memory write denied by policy \u2014 use the memory tool; direct file edits bypass locking, bounds, and validation."
			}
			if isTopLevelDir(rel, "skills") {
				return "Skill write denied by policy — use the skill_manage tool; direct file edits bypass validation."
			}
			if hasPathComponent(rel, "cache") {
				return "Write denied: '" + rel + "' is managed cache; write temporary files under 'cache/scratch/'."
			}
			return "Write denied: '" + rel + "' is system-managed and read-only."
		}
		return "path is not accessible"
	default:
		return "path is not accessible"
	}
}

// Classify assigns an absolute path to a file group for one workspace root.
// Anything outside the root is cross-profile or outside the data tree:
// protected names stay sealed, everything else is system-managed.
func Classify(abs, root string) Group {
	abs = filepath.Clean(abs)
	root = filepath.Clean(root)
	if isProtectedName(abs) {
		return GroupProtected
	}
	cross := !underPath(abs, root)
	if !cross {
		// The default profile's root contains every named profile: paths
		// under profiles/ are cross-profile even though they sit inside
		// the root. (A named profile has no profiles/ child, so the
		// prefix only ever matches its own subtree, where system
		// read-only is the safe side.)
		if rel, err := filepath.Rel(root, abs); err == nil &&
			(rel == "profiles" || strings.HasPrefix(rel, "profiles"+string(filepath.Separator))) {
			cross = true
		}
	}
	if isStateDBBase(filepath.Base(abs)) {
		if cross {
			return GroupProtected
		}
		return GroupSystem
	}
	if hasPathComponent(abs, "memories") {
		return GroupSystem
	}
	// The profile skills tree is tool-managed like memories: reads roam,
	// writes go through the skill tool. Only the top-level tree matches so
	// unrelated nested skills/ directories stay agent-owned.
	if underPath(abs, filepath.Join(root, "skills")) {
		return GroupSystem
	}
	if filepath.Base(abs) == "SOUL.md" {
		if cross {
			return GroupSystem
		}
		return GroupAgent
	}
	if !cross {
		if underPath(abs, filepath.Join(root, "cache", "scratch")) {
			return GroupAgent
		}
		if underPath(abs, filepath.Join(root, "cache")) {
			return GroupSystem
		}
		return GroupAgent
	}
	// Outside the workspace root: the container user is the backstop. Only
	// per-process secret vectors stay sealed (environment and command
	// lines can carry credentials); telemetry elsewhere under /proc, /sys,
	// and /dev resolves system-managed.
	if underPath(abs, "/proc") {
		switch filepath.Base(abs) {
		case "environ", "cmdline":
			return GroupProtected
		}
	}
	return GroupSystem
}

func underPath(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func isStateDBBase(base string) bool {
	switch base {
	case "state.db", "state.db-wal", "state.db-shm", "state.db-journal", "state.db-journal-mode":
		return true
	}
	return false
}

// isProtectedName reports secret- or operator-bearing names at any depth:
// dotenv files, profile configuration, credential stores, and private keys.
// Callers classify basenames and the mcp-tokens directory alike.
func isProtectedName(abs string) bool {
	base := filepath.Base(abs)
	if base == "config.yaml" {
		return true
	}
	if base == ".env" || strings.HasPrefix(base, ".env") {
		return true
	}
	if base == "credentials" || strings.HasPrefix(base, "credentials.") ||
		strings.HasSuffix(base, "credentials.json") ||
		strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
		return true
	}
	return hasPathComponent(abs, "mcp-tokens")
}

func hasPathComponent(path, name string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == name {
			return true
		}
	}
	return false
}

// isTopLevelDir reports whether rel sits directly under the top-level name
// directory (rel itself or its first component), using either separator so
// display paths from any platform match.
func isTopLevelDir(rel, name string) bool {
	rel = filepath.Clean(rel)
	if rel == name {
		return true
	}
	return strings.HasPrefix(rel, name+string(filepath.Separator)) || strings.HasPrefix(rel, name+"/")
}
