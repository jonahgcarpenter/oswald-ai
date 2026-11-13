package writefile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

// NewWriteFileHandler returns the whole-file writer with a read-before-write gate.
func NewWriteFileHandler(ws *workspace.Workspace) func(context.Context, map[string]interface{}) (governance.Result, error) {
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
		abs, rel, err := ws.ResolveWrite(rawPath)
		if err != nil {
			if drel, group, ok := workspace.AsDeny(err); ok {
				return governance.Result{}, errors.New(workspace.DenyMessage(drel, group, true))
			}
			return governance.Result{}, err
		}
		reqID := requestID(ctx)
		oldData, readErr := workspace.ReadRegularFile(abs)
		exists := readErr == nil
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return governance.Result{}, errors.New("file is unavailable")
		}
		if exists {
			oldHash := workspace.FileHash(oldData)
			if !ws.HasFullRead(reqID, abs, oldHash) {
				return governance.Result{}, errors.New("write refused: read_file the full file first, merge, then retry")
			}
		}
		if err := workspace.CheckSyntax(abs, string(oldData), content); err != nil {
			return governance.Result{}, err
		}
		if err := workspace.WriteRegularFile(abs, content); err != nil {
			return governance.Result{}, errors.New("file is unavailable")
		}
		data, err := workspace.ReadRegularFile(abs)
		if err != nil {
			return governance.Result{}, errors.New("file is unavailable")
		}
		sum := sha256.Sum256(data)
		verified := hex.EncodeToString(sum[:])
		if workspace.FileHash([]byte(content)) != workspace.FileHash(data) {
			return governance.Result{}, errors.New("write verification failed")
		}
		ws.NoteWrite(reqID, abs, workspace.FileHash(data))
		return governance.Result{
			Content: fmt.Sprintf("Wrote %s (%d bytes, verified:%s)", rel, len(data), verified[:12]),
			Outcome: governance.OutcomeProductive,
		}, nil
	}
}

// The arg helpers below are intentionally local: every tools/* package owns
// its own small copies instead of a shared generic helper package.

func rejectUnknown(args map[string]interface{}, allowed ...string) error {
	keep := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		keep[k] = true
	}
	for k := range args {
		if !keep[k] {
			return fmt.Errorf("unknown argument %q", k)
		}
	}
	return nil
}

func requestID(ctx context.Context) string {
	return strings.TrimSpace(requestctx.MetadataFromContext(ctx).RequestID)
}

func stringArg(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key].(string)
	return v, ok
}
