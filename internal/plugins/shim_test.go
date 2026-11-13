package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shimProfile(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(PluginCodeDir(root, "demo"), 0755); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeShimManifest(t *testing.T, root, name, body string) {
	t.Helper()
	dir := PluginCodeDir(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeShimBin(t *testing.T, root, name, bin string) {
	t.Helper()
	binDir := filepath.Join(PluginCodeDir(root, name), "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, bin), []byte("#!/bin/sh\necho synthetic\n"), 0755); err != nil {
		t.Fatal(err)
	}
}

const shimManifest = "name: demo\nversion: \"1.0\"\ndescription: synthetic shim plugin\nmanifest_version: 2\napi_version: \"1\"\nnode_package: demo\nnode_version: 1.2.3\n"

func TestRegenerateShimsCreatesRunnableWrapper(t *testing.T) {
	root := shimProfile(t)
	writeShimManifest(t, root, "demo", shimManifest)
	writeShimBin(t, root, "demo", "demo")
	if err := RegenerateShims(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(PluginShimDir(root), "demo")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("shim missing: %v", err)
	}
	content := string(data)
	if !strings.HasPrefix(content, "#!/bin/sh\n") {
		t.Fatalf("shim missing shebang:\n%s", content)
	}
	if !strings.Contains(content, "data"+string(filepath.Separator)+"home") {
		t.Fatalf("shim missing data/home redirect:\n%s", content)
	}
	if !strings.Contains(content, "node_modules") {
		t.Fatalf("shim missing absolute target:\n%s", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Fatal("shim is not executable")
	}
	if _, err := os.Stat(filepath.Join(PluginDataDir(root, "demo"), "home")); err != nil {
		t.Fatalf("data home not prepared: %v", err)
	}
}

func TestRegenerateShimsSkipsUnknownBin(t *testing.T) {
	root := shimProfile(t)
	// Lockfile-only shape with no known bin: no shim.
	writeShimManifest(t, root, "demo", "name: demo\nversion: \"1.0\"\ndescription: synthetic\nmanifest_version: 2\napi_version: \"1\"\n")
	if err := RegenerateShims(root); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(PluginShimDir(root)); err != nil || len(entries) != 0 {
		t.Fatalf("unexpected shims: %v %v", entries, err)
	}
}

func TestRegenerateShimsCollisionFirstWins(t *testing.T) {
	root := shimProfile(t)
	writeShimManifest(t, root, "aaa", "name: aaa\nversion: \"1.0\"\ndescription: synthetic\nmanifest_version: 2\napi_version: \"1\"\nnode_package: shared\nnode_version: 1.0.0\nnode_bin: tool\n")
	writeShimBin(t, root, "aaa", "tool")
	writeShimManifest(t, root, "zzz", "name: zzz\nversion: \"1.0\"\ndescription: synthetic\nmanifest_version: 2\napi_version: \"1\"\nnode_package: shared\nnode_version: 1.0.0\nnode_bin: tool\n")
	writeShimBin(t, root, "zzz", "tool")
	if err := RegenerateShims(root); err != nil {
		t.Fatal(err)
	}
	names, err := shimStats(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "tool" {
		t.Fatalf("shims = %v, want [tool]", names)
	}
	data, err := os.ReadFile(filepath.Join(PluginShimDir(root), "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), filepath.Join("aaa", "code")) {
		t.Fatalf("collision winner is not aaa:\n%s", data)
	}
}

func TestRegenerateShimsPrunesStale(t *testing.T) {
	root := shimProfile(t)
	writeShimManifest(t, root, "demo", shimManifest)
	writeShimBin(t, root, "demo", "demo")
	if err := RegenerateShims(root); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(PluginCodeDir(root, "demo")); err != nil {
		t.Fatal(err)
	}
	if err := RegenerateShims(root); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(PluginShimDir(root)); err != nil || len(entries) != 0 {
		t.Fatalf("stale shim survived: %v %v", entries, err)
	}
}

func TestRegenerateShimsRejectsEscape(t *testing.T) {
	root := shimProfile(t)
	writeShimManifest(t, root, "demo", "name: demo\nversion: \"1.0\"\ndescription: synthetic\nmanifest_version: 2\napi_version: \"1\"\nentry: ../../escape\n")
	if err := RegenerateShims(root); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(PluginShimDir(root)); err != nil || len(entries) != 0 {
		t.Fatalf("escape entry produced a shim: %v %v", entries, err)
	}
}

func TestRegenerateShimsEntryTarget(t *testing.T) {
	root := shimProfile(t)
	writeShimManifest(t, root, "demo", "name: demo\nversion: \"1.0\"\ndescription: synthetic\nmanifest_version: 2\napi_version: \"1\"\nentry: bin/demo\n")
	binPath := filepath.Join(PluginCodeDir(root, "demo"), "bin", "demo")
	if err := os.MkdirAll(filepath.Dir(binPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho synthetic\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := RegenerateShims(root); err != nil {
		t.Fatal(err)
	}
	// Entry plugins are addressed by plugin key.
	if _, err := os.Stat(filepath.Join(PluginShimDir(root), "demo")); err != nil {
		t.Fatalf("entry shim missing: %v", err)
	}
}
