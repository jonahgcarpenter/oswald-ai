package config

import (
	"os"
	"path/filepath"
	"testing"
)

const classifyFixture = `providers:
  fake:
    api: http://127.0.0.1:9999/v1
    key: test-key
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  api:
    enabled: true
    extra:
      api_host: 127.0.0.1
      api_port: 12345
runtime:
  log_level: info
`

func classifyRoot(t *testing.T, text string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCommitRejectsInvalidCandidate(t *testing.T) {
	root := classifyRoot(t, classifyFixture)
	path := filepath.Join(root, "config.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte("platforms: []\n")
	if err := CommitConfigCandidate(root, path, bad, before); err == nil {
		t.Fatal("invalid candidate accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("file mutated on failed validation")
	}
	if _, err := os.Stat(LastKnownGoodPath(path)); !os.IsNotExist(err) {
		t.Fatal("LKG written on failed validation")
	}
}

func TestCommitWritesLKGOnSuccess(t *testing.T) {
	root := classifyRoot(t, classifyFixture)
	path := filepath.Join(root, "config.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	good := []byte(classifyFixture + "# touch\n")
	// Trailing comment keeps the document valid while changing bytes.
	candidate := append([]byte(classifyFixture), []byte("# touch\n")...)
	_ = good
	if err := CommitConfigCandidate(root, path, candidate, before); err != nil {
		t.Fatalf("valid commit rejected: %v", err)
	}
	lkg, err := ReadLastKnownGood(path)
	if err != nil {
		t.Fatalf("LKG missing: %v", err)
	}
	if string(lkg) != string(before) {
		t.Fatal("LKG does not hold previous bytes")
	}
}

func TestClassifyReloadHotAndRestart(t *testing.T) {
	base, err := Validate(classifyRoot(t, classifyFixture))
	if err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	hot, classes := ClassifyReload(base, base)
	if !hot || len(classes) != 0 {
		t.Fatalf("identical = hot=%v classes=%v, want hot no classes", hot, classes)
	}
	if got := ReloadEffect(base, base); got != "live" {
		t.Fatalf("identical effect = %q, want live", got)
	}
	changed, err := Validate(classifyRoot(t, classifyFixture+"mcp_servers:\n  notes:\n    url: https://example.com/mcp\n"))
	if err != nil {
		t.Fatalf("variant invalid: %v", err)
	}
	hot, classes = ClassifyReload(base, changed)
	if hot {
		t.Fatal("mcp change reported hot, want restart")
	}
	found := false
	for _, c := range classes {
		if c == "mcp_servers" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mcp_servers class missing in %v", classes)
	}
	if got := ReloadEffect(base, changed); got != "restart" {
		t.Fatalf("mcp effect = %q, want restart", got)
	}
}
