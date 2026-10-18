package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// NewWriteFileHandler returns the whole-file writer with a read-before-write gate.
func NewWriteFileHandler(ws *Workspace) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		if ws == nil || ws.Root() == "" {
			return governance.Result{}, errors.New("workspace is unavailable")
		}
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("workspace: authenticated principal required")
		}
		if err := rejectUnknown(args, "path", "content"); err != nil {
			return governance.Result{}, err
		}
		rawPath, ok := stringArg(args, "path")
		if !ok || strings.TrimSpace(rawPath) == "" {
			return governance.Result{}, errors.New("path is required")
		}
		content, ok := stringArg(args, "content")
		if !ok {
			return governance.Result{}, errors.New("content must be a string")
		}
		if len(content) > 2<<20 {
			return governance.Result{}, errors.New("content exceeds size limit")
		}
		abs, _, err := ws.Resolve(rawPath)
		if err != nil {
			if rel, category, ok := AsDeny(err); ok {
				return governance.Result{}, errors.New(DenyMessage(rel, category, true))
			}
			return governance.Result{}, err
		}
		reqID := requestID(ctx)
		oldData, readErr := readRegularFile(abs)
		exists := readErr == nil
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return governance.Result{}, errors.New("file is unavailable")
		}
		if exists {
			oldHash := FileHash(oldData)
			if !ws.HasFullRead(reqID, abs, oldHash) {
				return governance.Result{}, errors.New("write refused: read_file the full file first, merge, then retry")
			}
		}
		if err := checkSyntax(abs, string(oldData), content); err != nil {
			return governance.Result{}, err
		}
		if err := writeRegularFile(abs, content); err != nil {
			return governance.Result{}, errors.New("file is unavailable")
		}
		data, err := readRegularFile(abs)
		if err != nil {
			return governance.Result{}, errors.New("file is unavailable")
		}
		sum := sha256.Sum256(data)
		verified := hex.EncodeToString(sum[:])
		if FileHash([]byte(content)) != FileHash(data) {
			return governance.Result{}, errors.New("write verification failed")
		}
		ws.NoteWrite(reqID, abs, FileHash(data))
		rel, _ := filepath.Rel(ws.Root(), abs)
		return governance.Result{
			Content: fmt.Sprintf("Wrote %s (%d bytes, verified:%s)", rel, len(data), verified[:12]),
			Outcome: governance.OutcomeProductive,
		}, nil
	}
}

func writeRegularFile(abs, content string) error {
	dir := filepath.Dir(abs)
	if err := mkdirPrivateAll(dir); err != nil {
		// Parent creation is best-effort private; fall back to standard mkdir.
		if mk := os.MkdirAll(dir, 0700); mk != nil {
			return mk
		}
	}
	tmp, err := os.CreateTemp(dir, ".workspace-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Refuse to overwrite symlinks.
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("refuses to overwrite special file")
		}
	}
	if err := os.Rename(name, abs); err != nil {
		return err
	}
	if err := os.Chmod(abs, 0600); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer d.Close()
	_ = d.Sync()
	_ = syscall.Fsync(int(d.Fd()))
	return nil
}
