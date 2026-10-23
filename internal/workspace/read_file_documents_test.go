package workspace

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
)

func TestReadCachedDocument(t *testing.T) {
	wsRoot := t.TempDir()
	if err := os.Chmod(wsRoot, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(wsRoot, "alice")
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
	ws := NewWorkspace(wsRoot, "alice")
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
	ws := NewWorkspace(root, "alice")
	read := NewReadFileHandler(ws)
	result, err := read(testContext("req-docx-1"), map[string]interface{}{"path": "report.docx"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(result.Content, "Workspace docx content") {
		t.Fatalf("missing content: %q", result.Content)
	}
}
