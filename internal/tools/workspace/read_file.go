package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

const (
	readCharBudget = 100_000
	defaultReadMax = 2000
)

// NewReadFileHandler returns the paginated text-file reader.
func NewReadFileHandler(ws *Workspace) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		return readFileCall(ctx, ws, args)
	}
}

func readFileCall(ctx context.Context, ws *Workspace, args map[string]interface{}) (governance.Result, error) {
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
	abs, _, err := ws.Resolve(rawPath)
	if err != nil {
		if suggestion := suggestSimilar(ws.Root(), rawPath); suggestion != "" {
			return governance.Result{}, fmt.Errorf("file not found; did you mean %s", suggestion)
		}
		return governance.Result{}, err
	}
	data, err := readRegularFile(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if suggestion := suggestSimilar(ws.Root(), rawPath); suggestion != "" {
				return governance.Result{}, fmt.Errorf("file not found; did you mean %s", suggestion)
			}
			return governance.Result{}, errors.New("file not found")
		}
		return governance.Result{}, errors.New("file is unavailable")
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
		ws.NoteFullRead(requestID(ctx), abs, FileHash(data))
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

func readRegularFile(abs string) ([]byte, error) {
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	// Bound single reads at 8 MiB; pagination handles the rest.
	const maxBytes = 8 << 20
	buf := make([]byte, 0, 64<<10)
	chunk := make([]byte, 32<<10)
	for {
		n, err := f.Read(chunk)
		if n > 0 {
			if len(buf)+n > maxBytes+1 {
				return nil, errors.New("file exceeds size limit")
			}
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
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
