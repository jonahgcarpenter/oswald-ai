package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileTree builds a fake .oswald tree: <tmp>/.oswald with global operator
// files and a named bob profile. Returns the tree root and bob's root.
func fileTree(t *testing.T) (tree, bob string) {
	t.Helper()
	tree = filepath.Join(t.TempDir(), ".oswald")
	bob = filepath.Join(tree, "profiles", "bob")
	for _, d := range []string{tree, filepath.Join(tree, "cache", "scratch"), filepath.Join(tree, "memories"), bob} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return tree, bob
}

func TestClassifyGroups(t *testing.T) {
	root := filepath.Join("tmp", ".oswald")
	cases := []struct {
		path string
		want Group
	}{
		// Protected everywhere, same or cross.
		{filepath.Join(root, ".env"), GroupProtected},
		{filepath.Join(root, ".env.local"), GroupProtected},
		{filepath.Join(root, "profiles", "bob", ".env"), GroupProtected},
		{filepath.Join(root, "deploy.pem"), GroupProtected},
		{filepath.Join(root, "id.key"), GroupProtected},
		{filepath.Join(root, "vault", "credentials.json"), GroupProtected},
		{filepath.Join(root, "mcp-tokens", "tok"), GroupProtected},
		{filepath.Join(root, "profiles", "bob", "mcp-tokens", "tok"), GroupProtected},
		// Outside the tree: virtual filesystems stay sealed.
		// Sealed: process secret vectors, even though /proc telemetry roams.
		{filepath.Join(string(filepath.Separator), "proc", "self", "environ"), GroupProtected},
		{filepath.Join(string(filepath.Separator), "proc", "1", "cmdline"), GroupProtected},
		// Roaming telemetry reads as system-managed.
		{filepath.Join(string(filepath.Separator), "proc", "cpuinfo"), GroupSystem},
		{filepath.Join(string(filepath.Separator), "sys", "x"), GroupSystem},
		{filepath.Join(string(filepath.Separator), "dev", "null"), GroupSystem},
		// state.db: system same-profile, protected cross-profile.
		{filepath.Join(root, "state.db"), GroupSystem},
		{filepath.Join(root, "state.db-wal"), GroupSystem},
		{filepath.Join(root, "profiles", "bob", "state.db"), GroupProtected},
		// memories: system everywhere, tool-mediated.
		{filepath.Join(root, "memories", "USER.md"), GroupSystem},
		{filepath.Join(root, "profiles", "bob", "memories", "MEMORY.md"), GroupSystem},
		// skills: system everywhere, tool-mediated; nested namesakes stay agent-owned.
		{filepath.Join(root, "skills", "review", "SKILL.md"), GroupSystem},
		{filepath.Join(root, "skills"), GroupSystem},
		{filepath.Join(root, "profiles", "bob", "skills", "review", "SKILL.md"), GroupSystem},
		{filepath.Join(root, "project", "skills", "notes.md"), GroupAgent},
		// SOUL.md: agent same-profile, system cross-profile.
		{filepath.Join(root, "SOUL.md"), GroupAgent},
		{filepath.Join(root, "profiles", "bob", "SOUL.md"), GroupSystem},
		// config.yaml: protected everywhere, CLI-mediated.
		{filepath.Join(root, "config.yaml"), GroupProtected},
		{filepath.Join(root, "profiles", "bob", "config.yaml"), GroupProtected},
		// cache: scratch is agent same-profile, everything else system.
		{filepath.Join(root, "cache", "scratch", "a.txt"), GroupAgent},
		{filepath.Join(root, "cache", "images", "a.png"), GroupSystem},
		{filepath.Join(root, "cache", "browser", "scratch-abc.txt"), GroupSystem},
		{filepath.Join(root, "cache", "browser", "profile-abc"), GroupSystem},
		{filepath.Join(root, "profiles", "bob", "cache", "scratch", "a.txt"), GroupSystem},
		{filepath.Join(root, "profiles", "bob", "notes.md"), GroupSystem},
		// Agent-owned and outside-tree reads.
		{filepath.Join(root, "notes.md"), GroupAgent},
		{filepath.Join(root, "sub", "dir", "f.txt"), GroupAgent},
		{filepath.Join(string(filepath.Separator), "etc", "hostname"), GroupSystem},
		{filepath.Join(string(filepath.Separator), "tmp", "x.txt"), GroupSystem},
	}
	for _, tc := range cases {
		if got := Classify(tc.path, root); got != tc.want {
			t.Errorf("Classify(%q) = %d, want %d", tc.path, got, tc.want)
		}
	}
}

func TestResolveReadsAndWritesByGroup(t *testing.T) {
	tree, _ := fileTree(t)
	for _, p := range []string{".env", "config.yaml", "SOUL.md", "state.db", filepath.Join("memories", "USER.md"), "notes.md", filepath.Join("profiles", "bob", "notes.md"), filepath.Join("skills", "review", "SKILL.md")} {
		full := filepath.Join(tree, p)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ws := NewWorkspace(tree, "default")

	readOK := []string{"SOUL.md", "state.db", filepath.Join("memories", "USER.md"), "notes.md", filepath.Join("profiles", "bob", "notes.md"), filepath.Join("profiles", "bob", "SOUL.md"), filepath.Join("skills", "review", "SKILL.md")}
	for _, raw := range readOK {
		if _, _, err := ws.Resolve(raw); err != nil {
			t.Errorf("read denied: %q: %v", raw, err)
		}
	}
	readDeny := []string{".env", "config.yaml", filepath.Join("profiles", "bob", ".env"), filepath.Join("profiles", "bob", "state.db")}
	for _, raw := range readDeny {
		_, _, err := ws.Resolve(raw)
		rel, group, ok := AsDeny(err)
		if err == nil || !ok || group != GroupProtected {
			t.Errorf("read allowed or wrong group: %q: %v group=%d ok=%v", raw, err, group, ok)
		} else if msg := DenyMessage(rel, group, false); !strings.Contains(msg, "protected") && !strings.Contains(msg, "session_search") {
			t.Errorf("read message wrong for %q: %q", raw, msg)
		}
	}
	// Outside reads resolve to the absolute path.
	if abs, display, err := ws.Resolve("/etc/hostname"); err != nil || abs != "/etc/hostname" || display != "/etc/hostname" {
		t.Errorf("outside read = %q %q %v", abs, display, err)
	}
	if _, _, err := ws.Resolve("/proc/self/environ"); err == nil {
		t.Error("proc read allowed")
	}

	writeOK := []string{"notes.md", "SOUL.md"}
	for _, raw := range writeOK {
		if _, _, err := ws.ResolveWrite(raw); err != nil {
			t.Errorf("write denied: %q: %v", raw, err)
		}
	}
	writeDeny := map[string]string{
		"config.yaml":                        "oswald providers",
		"state.db":                           "system-managed",
		filepath.Join("memories", "USER.md"): "memory tool",
		filepath.Join("skills", "review", "SKILL.md"): "skill_manage tool",
		".env": "oswald env",
		filepath.Join("profiles", "bob", "notes.md"): "system-managed",
		"/tmp/outside.txt":                           "system-managed",
	}
	for raw, want := range writeDeny {
		_, _, err := ws.ResolveWrite(raw)
		rel, group, ok := AsDeny(err)
		if err == nil || !ok {
			t.Errorf("write allowed: %q", raw)
			continue
		}
		if msg := DenyMessage(rel, group, true); !strings.Contains(msg, want) {
			t.Errorf("write message for %q = %q, want %q", raw, msg, want)
		}
	}
}

func TestDenyMessagesGrouped(t *testing.T) {
	if msg := DenyMessage(".env", GroupProtected, false); !strings.Contains(msg, "oswald env show") {
		t.Fatalf("protected read: %q", msg)
	}
	if msg := DenyMessage(".env", GroupProtected, true); !strings.Contains(msg, "oswald env set") {
		t.Fatalf("protected write: %q", msg)
	}
	if msg := DenyMessage("profiles/b/state.db", GroupProtected, false); !strings.Contains(msg, "session_search") {
		t.Fatalf("cross state read: %q", msg)
	}
	if msg := DenyMessage("config.yaml", GroupProtected, true); !strings.Contains(msg, "oswald providers|model|api|gateway|platforms|tools|runtime|mcp") {
		t.Fatalf("config write: %q", msg)
	}
	if msg := DenyMessage("config.yaml", GroupProtected, false); !strings.Contains(msg, "oswald config show") {
		t.Fatalf("config read: %q", msg)
	}
	if msg := DenyMessage("memories/USER.md", GroupSystem, true); !strings.Contains(msg, "memory tool") {
		t.Fatalf("memory write: %q", msg)
	}
	if msg := DenyMessage(filepath.Join("skills", "review", "SKILL.md"), GroupSystem, true); !strings.Contains(msg, "skill_manage tool") {
		t.Fatalf("skill write: %q", msg)
	}
	if msg := DenyMessage("cache/images/a.png", GroupSystem, true); !strings.Contains(msg, "cache/scratch") {
		t.Fatalf("cache write: %q", msg)
	}
}
