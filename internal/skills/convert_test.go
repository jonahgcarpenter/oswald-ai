package skills

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// converterPaths locates the repo-root skill converter from this test file.
func converterPaths(t *testing.T) (script, stage, validate string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	script = filepath.Join(root, "library", "skills", "skill-convert", "scripts", "convert.py")
	stage = filepath.Join(root, "library", "skills", "skill-convert", "scripts", "stage.sh")
	validate = filepath.Join(root, "library", "skills", "skill-convert", "scripts", "validate.sh")
	for _, p := range []string{script, stage, validate} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("converter script missing: %s", p)
		}
	}
	return script, stage, validate
}

func requirePython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	return python
}

func runScript(t *testing.T, name string, args ...string) (string, int) {
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

func TestConvertOpenCodeSkills(t *testing.T) {
	python := requirePython(t)
	script, stage, validate := converterPaths(t)
	_, thisFile, _, _ := runtime.Caller(0)
	fixture := filepath.Join(filepath.Dir(thisFile), "testdata", "converter", "opencode")
	if out, code := runScript(t, "sh", stage, fixture); code != 0 {
		t.Fatalf("stage: %s", out)
	}
	out := filepath.Join(t.TempDir(), "out-oc")
	if output, code := runScript(t, python, script, "--source", fixture, "--out", out, "--origin-url", "https://example.com/oc"); code != 0 {
		t.Fatalf("convert: %s", output)
	}
	if out, code := runScript(t, "sh", validate, out); code != 0 {
		t.Fatalf("validate: %s", out)
	}
	// Grouped dir keeps its nesting; flat file becomes a top-level skill.
	for _, dir := range []string{
		filepath.Join(out, "teams", "review"),
		filepath.Join(out, "build"),
	} {
		if _, err := ValidateBundle(dir); err != nil {
			t.Fatalf("emitted bundle invalid %s: %v", dir, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(out, "teams", "review", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "autoinvoke") {
		t.Fatalf("foreign flag leaked:\n%s", body)
	}
	if !strings.Contains(string(body), "name: review") {
		t.Fatalf("skill id not normalized:\n%s", body)
	}
	raw, err := os.ReadFile(filepath.Join(out, "conversion.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Skills []struct {
			Name string `json:"name"`
		} `json:"skills"`
		Dropped []string `json:"dropped_fields"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Skills) != 2 {
		t.Fatalf("record = %+v", record)
	}
}

func TestConvertHermesSkill(t *testing.T) {
	python := requirePython(t)
	script, _, validate := converterPaths(t)
	_, thisFile, _, _ := runtime.Caller(0)
	fixture := filepath.Join(filepath.Dir(thisFile), "testdata", "converter", "hermes")
	out := filepath.Join(t.TempDir(), "out-he")
	if output, code := runScript(t, python, script, "--source", fixture, "--out", out); code != 0 {
		t.Fatalf("convert: %s", output)
	}
	if out, code := runScript(t, "sh", validate, out); code != 0 {
		t.Fatalf("validate: %s", out)
	}
	skillDir := filepath.Join(out, "research", "arxiv")
	if _, err := ValidateBundle(skillDir); err != nil {
		t.Fatalf("emitted bundle invalid: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
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
	if _, err := os.Stat(filepath.Join(skillDir, "scripts", "search.py")); err != nil {
		t.Fatalf("supporting script not copied: %v", err)
	}
}

func TestConvertRefusesHostile(t *testing.T) {
	python := requirePython(t)
	script, stage, _ := converterPaths(t)
	_, thisFile, _, _ := runtime.Caller(0)
	// Symlink in source fails the stage gate and the converter.
	hostile := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostile, "SKILL.md"), []byte("---\nname: ok\ndescription: synthetic ok skill\n---\nbody\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(hostile, "SKILL.md"), filepath.Join(hostile, "link.md")); err != nil {
		t.Fatal(err)
	}
	if _, code := runScript(t, "sh", stage, hostile); code == 0 {
		t.Fatal("stage accepted symlink")
	}
	if _, code := runScript(t, python, script, "--source", hostile, "--out", filepath.Join(t.TempDir(), "out")); code == 0 {
		t.Fatal("convert accepted symlink")
	}
	// Unclosed fence converts nothing and fails.
	bad := filepath.Join(filepath.Dir(thisFile), "testdata", "converter", "badonly")
	if output, code := runScript(t, python, script, "--source", bad, "--out", filepath.Join(t.TempDir(), "out-bad")); code == 0 {
		t.Fatalf("convert accepted fenceless skill: %s", output)
	}
	// Invalid directory name must be renamed explicitly via --name.
	invalid := filepath.Join(filepath.Dir(thisFile), "testdata", "converter", "badname")
	if _, code := runScript(t, python, script, "--source", invalid, "--out", filepath.Join(t.TempDir(), "out-rename")); code == 0 {
		t.Fatal("convert accepted invalid skill name without --name")
	}
	renamed := filepath.Join(t.TempDir(), "out-renamed")
	if output, code := runScript(t, python, script, "--source", invalid, "--out", renamed, "--name", "goodname"); code != 0 {
		t.Fatalf("convert with --name: %s", output)
	}
	if _, err := ValidateBundle(filepath.Join(renamed, "goodname")); err != nil {
		t.Fatalf("renamed bundle invalid: %v", err)
	}
}
