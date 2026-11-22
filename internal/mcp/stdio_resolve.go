package mcp

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// launcherBase reports the executable name for a configured stdio command,
// folding portable Windows spellings. Bare names resolve against the copies
// shipped in the container image; an absolute command stays the operator's
// choice.
func launcherBase(command string) string {
	base := path.Base(strings.TrimSpace(command))
	if i := strings.LastIndex(base, "."); i > 0 {
		if ext := strings.ToLower(base[i:]); ext == ".cmd" || ext == ".exe" {
			base = base[:i]
		}
	}
	return base
}

// npmCacheRoots lists npm cache roots for npx resolution, most profile-local
// first: the acting profile's own ~/.npm (so installs made inside the sandbox
// feed lazily connected servers), then the service process override, then the
// service user's default.
func npmCacheRoots(profileRoot string) []string {
	var roots []string
	seen := map[string]bool{}
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		roots = append(roots, dir)
	}
	if root := strings.TrimSpace(profileRoot); root != "" {
		add(filepath.Join(root, ".npm"))
	}
	add(os.Getenv("npm_config_cache"))
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".npm"))
	}
	return roots
}

// profileLocalBin resolves a bare stdio command against the acting
// profile's ~/.local/bin — the same user-install directory the sandbox PATH
// puts first — so agent installs launch servers by bare name. Absolute or
// relative spellings and missing or non-executable files return "" so the
// caller keeps its own resolution and error naming.
func profileLocalBin(command, profileRoot string) string {
	name := strings.TrimSpace(command)
	root := strings.TrimSpace(profileRoot)
	if name == "" || root == "" || strings.ContainsRune(name, '/') ||
		strings.ContainsRune(name, os.PathSeparator) || strings.ContainsRune(name, '\\') {
		return ""
	}
	candidate := filepath.Join(root, ".local", "bin", name)
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return candidate
}

// npxCachedBin resolves `npx -y <pkg> ...` to the already-installed binary in
// an npm cache, checking the acting profile's cache first. It stays
// conservative by design: a cache miss, version pin, extra npx flag,
// non-registry spec, or ambiguous bin manifest returns empty so the caller
// keeps plain npx and a cold machine still installs.
func npxCachedBin(args []string, profileRoot string) (string, []string) {
	rest := append([]string(nil), args...)
	for len(rest) > 0 && (rest[0] == "-y" || rest[0] == "--yes") {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return "", nil
	}
	// A flag after the spec would reach the server when npx would have eaten
	// it, so only a clean package spec resolves.
	for _, arg := range rest[1:] {
		if arg == "-y" || arg == "--yes" {
			return "", nil
		}
	}
	spec := rest[0]
	if spec == "" || strings.HasPrefix(spec, "-") || strings.Contains(spec, ":") ||
		strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") ||
		strings.HasPrefix(spec, "\\") || strings.HasPrefix(spec, "~") {
		return "", nil
	}
	// Scoped names keep their leading @; an @ after the scope is a pin.
	if strings.Contains(strings.TrimPrefix(spec, "@"), "@") {
		return "", nil
	}
	// Bare names with a slash are not registry specs npx would cache by name.
	if strings.Contains(spec, "/") && !strings.HasPrefix(spec, "@") {
		return "", nil
	}
	for _, root := range npmCacheRoots(profileRoot) {
		npxRoot := filepath.Join(root, "_npx")
		entries, err := os.ReadDir(npxRoot)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if !npxCacheHasSpec(filepath.Join(npxRoot, entry.Name()), spec) {
				continue
			}
			bin, ok := npxCacheBin(filepath.Join(npxRoot, entry.Name()), spec)
			if !ok {
				continue
			}
			return bin, append([]string(nil), rest[1:]...)
		}
	}
	return "", nil
}

// npxCacheHasSpec reports whether an npx cache entry installed spec.
func npxCacheHasSpec(entryDir, spec string) bool {
	raw, err := os.ReadFile(filepath.Join(entryDir, "package.json"))
	if err != nil {
		return false
	}
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return false
	}
	_, ok := manifest.Dependencies[spec]
	return ok
}

// npxCacheBin locates the single executable bin a cached spec provides.
func npxCacheBin(entryDir, spec string) (string, bool) {
	pkgDir := filepath.Join(entryDir, "node_modules", filepath.FromSlash(spec))
	raw, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return "", false
	}
	var manifest struct {
		Bin json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest.Bin) == 0 {
		return "", false
	}
	var name string
	var asString string
	var asMap map[string]string
	switch {
	case json.Unmarshal(manifest.Bin, &asString) == nil:
		name = path.Base(spec)
	case json.Unmarshal(manifest.Bin, &asMap) == nil:
		if len(asMap) != 1 {
			return "", false
		}
		for key := range asMap {
			name = key
		}
	default:
		return "", false
	}
	if name == "" {
		return "", false
	}
	candidate := filepath.Join(entryDir, "node_modules", ".bin", name)
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	return candidate, true
}
