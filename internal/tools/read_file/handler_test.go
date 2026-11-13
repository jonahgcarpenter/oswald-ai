package readfile

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/docache"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

func testContext(requestID string) context.Context {
	ctx := context.Background()
	ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "alice", Assurance: identity.AssuranceDiscordGateway})
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: requestID})
	return ctx
}

func TestReadCachedDocument(t *testing.T) {
	wsRoot := t.TempDir()
	if err := os.Chmod(wsRoot, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(wsRoot, "alice")
	cacheRoot := t.TempDir()
	docs := docache.New(cacheRoot)
	ctx := testContext("req-doc-1")
	path, err := docs.Save(ctx, "alice", "notes.txt", []byte("cached hello\nsecond line\n"))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	read := NewReadFileHandlerWithDocCache(ws, docs)
	result, err := read(ctx, map[string]interface{}{"path": path})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(result.Content, "cached hello") {
		t.Fatalf("missing content: %q", result.Content)
	}
}

func TestReadCachedDocumentEnforcesProfile(t *testing.T) {
	wsRoot := t.TempDir()
	if err := os.Chmod(wsRoot, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(wsRoot, "alice")
	cacheRoot := t.TempDir()
	docs := docache.New(cacheRoot)
	saver := testContext("req-doc-2")
	path, err := docs.Save(saver, "alice", "notes.txt", []byte("secret"))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	bobCtx := requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "bob", Gateway: "discord", ExternalID: "bob", Assurance: identity.AssuranceDiscordGateway})
	bobCtx = requestctx.WithMetadata(bobCtx, requestctx.Metadata{RequestID: "req-doc-3"})
	read := NewReadFileHandlerWithDocCache(ws, docs)
	if _, err := read(bobCtx, map[string]interface{}{"path": path}); err == nil {
		t.Fatal("cross-profile document read should fail")
	}
}

func TestReadWorkspaceDOCX(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte(`<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Workspace docx content</w:t></w:r></w:p></w:body></w:document>`))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report.docx"), buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	read := NewReadFileHandler(ws)
	result, err := read(testContext("req-docx-1"), map[string]interface{}{"path": "report.docx"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(result.Content, "Workspace docx content") {
		t.Fatalf("missing content: %q", result.Content)
	}
}

func TestDynamicDescriptionOCROverride(t *testing.T) {
	SetOCRAvailable(false)
	base := Definition().Description
	if got := DynamicDescription(Name, base); got != base {
		t.Fatal("override applied without OCR")
	}
	SetOCRAvailable(true)
	defer SetOCRAvailable(false)
	got := DynamicDescription(Name, base)
	if !strings.Contains(got, "PDF (scanned or text)") || strings.Contains(got, "PDF (text layer)") {
		t.Fatalf("ocr override = %q", got)
	}
}

// The read-before-write gate lives in write_file, but the read half moved
// here: a write without a current full read fails, then succeeds after one.
func TestReadWriteGate(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	ctx := testContext("req-1")
	write := workspace.NewWriteFileHandler(ws)
	read := NewReadFileHandler(ws)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := write(ctx, map[string]interface{}{"path": "a.txt", "content": "bye"}); err == nil {
		t.Fatal("write without read succeeded")
	}
	if _, err := read(ctx, map[string]interface{}{"path": "a.txt"}); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	res, err := write(ctx, map[string]interface{}{"path": "a.txt", "content": "bye"})
	if err != nil || !strings.Contains(res.Content, "verified") {
		t.Fatalf("write after read = %+v %v", res, err)
	}
	// External modification invalidates the gate.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := write(ctx, map[string]interface{}{"path": "a.txt", "content": "again"}); err == nil {
		t.Fatal("stale write succeeded after external change")
	}
}

// Deny categories hold across both file surfaces: protected names seal on
// read and write, system-managed names redirect to their owning tool.
func TestDenyMessagesCategorized(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "default")
	read := NewReadFileHandler(ws)
	write := workspace.NewWriteFileHandler(ws)
	for _, p := range []string{"config.yaml", "SOUL.md", "state.db", filepath.Join("memories", "MEMORY.md"), filepath.Join("profiles", "alice", "x"), filepath.Join("skills", "review", "SKILL.md")} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("data\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		call string
		args map[string]interface{}
		want string
	}{
		{"read", map[string]interface{}{"path": ".env.local"}, "protected"},
		{"read", map[string]interface{}{"path": "profiles/alice/state.db"}, "session_search"},
		{"read", map[string]interface{}{"path": "config.yaml"}, "oswald config"},
		{"read", map[string]interface{}{"path": "profiles/alice/x"}, ""},
		{"read", map[string]interface{}{"path": "memories/MEMORY.md"}, ""},
		{"read", map[string]interface{}{"path": "skills/review/SKILL.md"}, ""},
		{"read", map[string]interface{}{"path": "SOUL.md"}, ""},
		{"write", map[string]interface{}{"path": "SOUL.md", "content": "x"}, ""},
		{"write", map[string]interface{}{"path": "config.yaml", "content": "x"}, "oswald providers"},
		{"write", map[string]interface{}{"path": "memories/MEMORY.md", "content": "x"}, "memory tool"},
		{"write", map[string]interface{}{"path": "skills/review/SKILL.md", "content": "x"}, "skill_manage tool"},
		{"write", map[string]interface{}{"path": "deploy.pem", "content": "x"}, "protected"},
		{"write", map[string]interface{}{"path": "profiles/alice/notes.md", "content": "x"}, "system-managed"},
	}
	for _, tc := range cases {
		var err error
		if tc.call == "read" {
			_, err = read(testContext("req-deny"), tc.args)
		} else {
			// Seed the read-before-write gate for agent-owned paths.
			if tc.want == "" {
				if _, rerr := read(testContext("req-deny"), map[string]interface{}{"path": tc.args["path"]}); rerr != nil {
					t.Fatalf("seed read %v = %v", tc.args["path"], rerr)
				}
			}
			_, err = write(testContext("req-deny"), tc.args)
		}
		if tc.want == "" {
			if err != nil {
				t.Fatalf("%s %v = %v, want success", tc.call, tc.args["path"], err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s %v = %v, want %q", tc.call, tc.args["path"], err, tc.want)
		}
	}
}
