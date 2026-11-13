package readfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/docache"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/docextract"
)

const (
	readCharBudget = 100_000
	defaultReadMax = 2000
)

// NewReadFileHandler returns the paginated text-file reader.
func NewReadFileHandler(ws *workspace.Workspace) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		return readFileCall(ctx, ws, nil, args)
	}
}

// NewReadFileHandlerWithDocCache returns the reader with private inbound
// document cache access. Cache paths named in prompts resolve here.
func NewReadFileHandlerWithDocCache(ws *workspace.Workspace, docs *docache.Cache) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		return readFileCall(ctx, ws, docs, args)
	}
}

func readFileCall(ctx context.Context, ws *workspace.Workspace, docs *docache.Cache, args map[string]interface{}) (governance.Result, error) {
	if err := ctx.Err(); err != nil {
		return governance.Result{}, err
	}
	if ws == nil || ws.Root() == "" {
		return governance.Result{}, errors.New("workspace is unavailable")
	}
	if err := rejectUnknown(args, "path", "offset", "limit"); err != nil {
		return governance.Result{}, err
	}
	rawPath, ok := stringArg(args, "path")
	if !ok || strings.TrimSpace(rawPath) == "" {
		return governance.Result{}, errors.New("path is required")
	}
	offset, err := intArg(args, "offset", 1)
	if err != nil || offset < 1 {
		return governance.Result{}, errors.New("offset must be an integer >= 1")
	}
	limit, err := intArg(args, "limit", defaultReadMax)
	if err != nil || limit < 1 || limit > defaultReadMax {
		return governance.Result{}, errors.New("limit must be an integer between 1 and 2000")
	}
	if result, err, ok := readCachedDocument(ctx, docs, strings.TrimSpace(rawPath), offset, limit); ok {
		return result, err
	}
	abs, _, err := ws.Resolve(rawPath)
	if err != nil {
		if rel, category, ok := workspace.AsDeny(err); ok {
			return governance.Result{}, errors.New(workspace.DenyMessage(rel, category, false))
		}
		if suggestion := suggestSimilar(ws.Root(), rawPath); suggestion != "" {
			return governance.Result{}, fmt.Errorf("file not found; did you mean %s", suggestion)
		}
		return governance.Result{}, err
	}
	data, err := workspace.ReadRegularFile(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if suggestion := suggestSimilar(ws.Root(), rawPath); suggestion != "" {
				return governance.Result{}, fmt.Errorf("file not found; did you mean %s", suggestion)
			}
			return governance.Result{}, errors.New("file not found")
		}
		return governance.Result{}, errors.New("file is unavailable")
	}
	if isBinaryContainer(abs) {
		return readExtractedWorkspaceFile(ctx, ws, abs, data, offset, limit)
	}
	if !utf8.Valid(data) {
		return governance.Result{}, errors.New("cannot read images/binary — use vision_analyze for images")
	}
	// Reject NUL-bearing text as binary.
	for _, b := range data {
		if b == 0 {
			return governance.Result{}, errors.New("cannot read images/binary — use vision_analyze for images")
		}
	}
	text := string(data)
	lines := strings.Split(text, "\n")
	// Drop the trailing empty element produced by a final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if offset > total+1 {
		return governance.Result{}, fmt.Errorf("offset exceeds file length (%d lines)", total)
	}
	end := offset - 1 + limit
	if end > total {
		end = total
	}
	var sb strings.Builder
	chars := 0
	lastIncluded := offset - 1
	truncated := false
	var nextOffset int
	for i := offset - 1; i < end; i++ {
		entry := fmt.Sprintf("%d|%s\n", i+1, lines[i])
		if chars+len(entry) > readCharBudget {
			truncated = true
			nextOffset = i + 1
			break
		}
		sb.WriteString(entry)
		chars += len(entry)
		lastIncluded = i + 1
	}
	content := strings.TrimSuffix(sb.String(), "\n")
	if !truncated {
		ws.NoteFullRead(requestID(ctx), abs, workspace.FileHash(data))
	} else {
		_ = lastIncluded
		out := content + fmt.Sprintf("\n[truncated at ~100K characters; continue with offset %d]", nextOffset)
		return governance.Result{Content: out, Outcome: governance.OutcomeProductive}, nil
	}
	if content == "" {
		return governance.Result{Content: "(empty file)", Outcome: governance.OutcomeProductive}, nil
	}
	return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
}

// isBinaryContainer reports whether abs needs document extraction instead of
// the raw text path.
func isBinaryContainer(abs string) bool {
	switch strings.ToLower(strings.TrimSpace(filepath.Ext(abs))) {
	case ".docx", ".xlsx", ".pdf":
		return true
	}
	return false
}

// readCachedDocument resolves a private inbound document cache path for the
// calling profile and renders its extracted text. ok is false when path is
// not a cache document, letting the workspace path take over.
func readCachedDocument(ctx context.Context, docs *docache.Cache, path string, offset, limit int) (governance.Result, error, bool) {
	if docs == nil || !filepath.IsAbs(path) {
		return governance.Result{}, nil, false
	}
	principal, ok := requestctx.PrincipalFromContext(ctx)
	if !ok || !principal.Authenticated() {
		return governance.Result{}, nil, false
	}
	data, _, err := docs.Resolve(ctx, principal.CanonicalUserID, path)
	if err != nil {
		if strings.Contains(path, string(filepath.Separator)+"cache"+string(filepath.Separator)+"documents"+string(filepath.Separator)) {
			return governance.Result{}, errors.New("file is unavailable"), true
		}
		return governance.Result{}, nil, false
	}
	text, err := docextract.Extract(path, data)
	if err != nil {
		return governance.Result{}, errors.New("document extraction failed — " + err.Error()), true
	}
	content, truncated, nextOffset, total := paginateExtracted(text, offset, limit)
	if offset > total+1 {
		return governance.Result{}, fmt.Errorf("offset exceeds file length (%d lines)", total), true
	}
	if truncated {
		return governance.Result{Content: content + fmt.Sprintf("\n[truncated at ~100K characters; continue with offset %d]", nextOffset), Outcome: governance.OutcomeProductive}, nil, true
	}
	if content == "" {
		return governance.Result{Content: "(empty file)", Outcome: governance.OutcomeProductive}, nil, true
	}
	return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil, true
}

// readExtractedWorkspaceFile renders a workspace binary container through the
// document extractor with the standard pagination envelope.
func readExtractedWorkspaceFile(ctx context.Context, ws *workspace.Workspace, abs string, data []byte, offset, limit int) (governance.Result, error) {
	text, err := docextract.Extract(abs, data)
	if err != nil {
		return governance.Result{}, errors.New("cannot read images/binary — use vision_analyze for images")
	}
	content, truncated, nextOffset, total := paginateExtracted(text, offset, limit)
	if offset > total+1 {
		return governance.Result{}, fmt.Errorf("offset exceeds file length (%d lines)", total)
	}
	if truncated {
		return governance.Result{Content: content + fmt.Sprintf("\n[truncated at ~100K characters; continue with offset %d]", nextOffset), Outcome: governance.OutcomeProductive}, nil
	}
	if content == "" {
		return governance.Result{Content: "(empty file)", Outcome: governance.OutcomeProductive}, nil
	}
	ws.NoteFullRead(requestID(ctx), abs, workspace.FileHash(data))
	return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
}

// paginateExtracted splits extracted text into numbered lines with the shared
// character budget.
func paginateExtracted(text string, offset, limit int) (string, bool, int, int) {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if offset > total+1 {
		return "", false, 0, total
	}
	end := offset - 1 + limit
	if end > total {
		end = total
	}
	var sb strings.Builder
	chars := 0
	truncated := false
	nextOffset := 0
	for i := offset - 1; i < end; i++ {
		entry := fmt.Sprintf("%d|%s\n", i+1, lines[i])
		if chars+len(entry) > readCharBudget {
			truncated = true
			nextOffset = i + 1
			break
		}
		sb.WriteString(entry)
		chars += len(entry)
	}
	return strings.TrimSuffix(sb.String(), "\n"), truncated, nextOffset, total
}

func suggestSimilar(root, raw string) string {
	trimmed := strings.TrimSpace(raw)
	base := filepath.Base(trimmed)
	dir := filepath.Dir(trimmed)
	var searchDir string
	if filepath.IsAbs(trimmed) {
		searchDir = filepath.Dir(trimmed)
	} else if dir == "." || dir == "" {
		searchDir = root
	} else {
		searchDir = filepath.Join(root, dir)
	}
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		entries, err = os.ReadDir(root)
		if err != nil {
			return ""
		}
	}
	best := ""
	bestScore := -1
	lowerBase := strings.ToLower(base)
	for _, e := range entries {
		name := e.Name()
		score := similarity(strings.ToLower(name), lowerBase)
		if score > bestScore {
			bestScore = score
			best = name
		}
	}
	if bestScore <= 0 || best == "" {
		return ""
	}
	// Only suggest when plausibly similar.
	if bestScore < 2 && !strings.Contains(strings.ToLower(best), lowerBase[:min(3, len(lowerBase))]) {
		return ""
	}
	if names, _ := filepath.Rel(root, filepath.Join(searchDir, best)); names != "" {
		return names
	}
	return best
}

func similarity(a, b string) int {
	if a == b {
		return 100
	}
	// Longest common prefix plus containment bonus.
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	score := n
	if strings.Contains(a, b) || strings.Contains(b, a) {
		score += 3
	}
	// Sorted candidate order keeps suggestions deterministic.
	_ = sort.StringSlice{}
	return score
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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

func intArg(args map[string]interface{}, key string, def int) (int, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return def, nil
	}
	switch v := raw.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		return int(v), nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("%s must be an integer", key)
	}
}
