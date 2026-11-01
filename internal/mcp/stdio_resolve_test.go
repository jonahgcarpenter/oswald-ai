package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCacheJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCacheBin(t *testing.T, path string) {
	t.Helper()
	writeCacheJSON(t, path, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLauncherBase(t *testing.T) {
	for input, want := range map[string]string{
		"npx":                "npx",
		"  npx  ":            "npx",
		"/usr/local/bin/npx": "npx",
		"npx.cmd":            "npx",
		"node.exe":           "node",
		"python3":            "python3",
	} {
		if got := launcherBase(input); got != want {
			t.Fatalf("launcherBase(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNpxCachedBinHit(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("npm_config_cache", cache)
	entry := filepath.Join(cache, "_npx", "abc123")
	writeCacheJSON(t, filepath.Join(entry, "package.json"), `{"dependencies":{"@scope/server":"*"}}`)
	writeCacheJSON(t, filepath.Join(entry, "node_modules", "@scope", "server", "package.json"),
		`{"bin":{"serve":"./dist/index.js"}}`)
	bin := filepath.Join(entry, "node_modules", ".bin", "serve")
	writeCacheBin(t, bin)
	got, rest := npxCachedBin([]string{"-y", "@scope/server", "--port", "1"})
	if got != bin {
		t.Fatalf("npxCachedBin = %q, want %q", got, bin)
	}
	if len(rest) != 2 || rest[0] != "--port" || rest[1] != "1" {
		t.Fatalf("npxCachedBin rest = %+v", rest)
	}
}

func TestNpxCachedBinStringBin(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("npm_config_cache", cache)
	entry := filepath.Join(cache, "_npx", "abc123")
	writeCacheJSON(t, filepath.Join(entry, "package.json"), `{"dependencies":{"server":"*"}}`)
	writeCacheJSON(t, filepath.Join(entry, "node_modules", "server", "package.json"),
		`{"bin":"./dist/index.js"}`)
	bin := filepath.Join(entry, "node_modules", ".bin", "server")
	writeCacheBin(t, bin)
	got, rest := npxCachedBin([]string{"--yes", "server"})
	if got != bin || len(rest) != 0 {
		t.Fatalf("npxCachedBin = %q %+v, want %q []", got, rest, bin)
	}
}

func TestNpxCachedBinMiss(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("npm_config_cache", cache)
	entry := filepath.Join(cache, "_npx", "abc123")
	writeCacheJSON(t, filepath.Join(entry, "package.json"), `{"dependencies":{"@scope/server":"*"}}`)
	writeCacheJSON(t, filepath.Join(entry, "node_modules", "@scope", "server", "package.json"),
		`{"bin":{"serve":"./dist/index.js"}}`)
	writeCacheBin(t, filepath.Join(entry, "node_modules", ".bin", "serve"))
	for _, args := range [][]string{
		nil,
		{"-y", "server@1.2.3"},
		{"-y", "@scope/server@1.2.3"},
		{"-y", "@scope/server", "-y"},
		{"-y", "https://example.com/server.tgz"},
		{"-y", "./local/server"},
		{"-y", "uninstalled"},
	} {
		if got, _ := npxCachedBin(args); got != "" {
			t.Fatalf("npxCachedBin(%+v) = %q, want miss", args, got)
		}
	}
}

func TestNpxCachedBinAmbiguousManifest(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("npm_config_cache", cache)
	entry := filepath.Join(cache, "_npx", "abc123")
	writeCacheJSON(t, filepath.Join(entry, "package.json"), `{"dependencies":{"server":"*"}}`)
	writeCacheJSON(t, filepath.Join(entry, "node_modules", "server", "package.json"),
		`{"bin":{"a":"./a.js","b":"./b.js"}}`)
	if got, _ := npxCachedBin([]string{"-y", "server"}); got != "" {
		t.Fatalf("multi-bin manifest must miss, got %q", got)
	}
}
