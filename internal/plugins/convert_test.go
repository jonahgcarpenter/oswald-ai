package plugins

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// converterPaths locates the repo-root converter skill from this test file.
func converterPaths(t *testing.T) (root, script, stage, validate string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	root = filepath.Dir(filepath.Dir(filepath.Dir(file)))
	script = filepath.Join(root, "library", "skills", "plugin-convert", "scripts", "convert.py")
	stage = filepath.Join(root, "library", "skills", "plugin-convert", "scripts", "stage.sh")
	validate = filepath.Join(root, "library", "skills", "plugin-convert", "scripts", "validate.sh")
	for _, p := range []string{script, stage, validate} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("converter script missing: %s", p)
		}
	}
	return root, script, stage, validate
}

func requirePython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	return python
}

func runCmd(t *testing.T, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("run %s: %v\n%s", name, err, output)
		}
	}
	return string(output), code
}

func TestConvertOpenCodeFixture(t *testing.T) {
	python := requirePython(t)
	_, script, stage, validate := converterPaths(t)
	_, thisFile, _, _ := runtime.Caller(0)
	fixture := filepath.Join(filepath.Dir(thisFile), "testdata", "converter", "opencode")
	if out, code := runCmd(t, "sh", stage, fixture); code != 0 {
		t.Fatalf("stage: %s", out)
	}
	out := filepath.Join(t.TempDir(), "out-oc")
	if output, code := runCmd(t, python, script, "--source", fixture, "--kind", "opencode", "--out", out, "--name", "ocdemo", "--origin-url", "https://example.com/oc", "--origin-rev", "abc123"); code != 0 {
		t.Fatalf("convert: %s", output)
	}
	if out, code := runCmd(t, "sh", validate, out); code != 0 {
		t.Fatalf("validate: %s", out)
	}
	manifest, err := ParseManifestFile(out, "profile")
	if err != nil {
		t.Fatalf("emitted manifest invalid: %v", err)
	}
	if manifest.Name != "ocdemo" {
		t.Fatalf("manifest name = %q", manifest.Name)
	}
	body, err := os.ReadFile(filepath.Join(out, "skills", "git-release", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "autoinvoke") {
		t.Fatalf("foreign flag leaked into skill:\n%s", body)
	}
	if !strings.Contains(string(body), "name: git-release") {
		t.Fatalf("skill id not normalized:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(out, "skills", "review", "SKILL.md")); err != nil {
		t.Fatalf("flat skill not converted: %v", err)
	}
	var record struct {
		Kind   string `json:"kind"`
		Plugin string `json:"plugin"`
		Skills []struct {
			Name string `json:"name"`
		} `json:"skills"`
	}
	raw, err := os.ReadFile(filepath.Join(out, "conversion.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Kind != "opencode" || len(record.Skills) != 2 {
		t.Fatalf("conversion record = %+v", record)
	}
}

func TestConvertHermesFixture(t *testing.T) {
	python := requirePython(t)
	_, script, _, _ := converterPaths(t)
	_, thisFile, _, _ := runtime.Caller(0)
	fixture := filepath.Join(filepath.Dir(thisFile), "testdata", "converter", "hermes")
	out := filepath.Join(t.TempDir(), "out-he")
	if output, code := runCmd(t, python, script, "--source", fixture, "--kind", "hermes", "--out", out, "--name", "hedemo"); code != 0 {
		t.Fatalf("convert: %s", output)
	}
	body, err := os.ReadFile(filepath.Join(out, "skills", "research", "arxiv", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "required_env: [ARXIV_API_KEY]") {
		t.Fatalf("env mapping missing:\n%s", text)
	}
	for _, want := range []string{"platforms", "blueprint", "required_environment_variables"} {
		if strings.Contains(text, want) {
			t.Fatalf("foreign field leaked %q:\n%s", want, text)
		}
	}
	if _, err := ParseManifestFile(out, "profile"); err != nil {
		t.Fatalf("emitted manifest invalid: %v", err)
	}
}

func TestConvertRefusesHostile(t *testing.T) {
	python := requirePython(t)
	_, script, stage, _ := converterPaths(t)
	// Symlink in source fails the stage gate.
	hostile := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostile, "SKILL.md"), []byte("---\nname: ok\ndescription: synthetic ok skill\n---\nbody\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(hostile, "SKILL.md"), filepath.Join(hostile, "link.md")); err != nil {
		t.Fatal(err)
	}
	if _, code := runCmd(t, "sh", stage, hostile); code == 0 {
		t.Fatal("stage accepted symlink")
	}
	if _, code := runCmd(t, python, script, "--source", hostile, "--kind", "hermes", "--out", filepath.Join(t.TempDir(), "out"), "--name", "hostile"); code == 0 {
		t.Fatal("convert accepted symlink")
	}
	// Unclosed fence converts nothing and fails.
	_, thisFile, _, _ := runtime.Caller(0)
	bad := filepath.Join(filepath.Dir(thisFile), "testdata", "converter", "badonly")
	if output, code := runCmd(t, python, script, "--source", bad, "--kind", "claude", "--out", filepath.Join(t.TempDir(), "out-bad"), "--name", "badonly"); code == 0 {
		t.Fatalf("convert accepted fenceless skill: %s", output)
	}
}
