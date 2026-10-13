package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// NewPatchHandler returns targeted find-and-replace edits.
func NewPatchHandler(ws *Workspace) func(context.Context, map[string]interface{}) (governance.Result, error) {
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
		if err := rejectUnknown(args, "path", "old_string", "new_string", "replace_all"); err != nil {
			return governance.Result{}, err
		}
		rawPath, ok := stringArg(args, "path")
		if !ok || strings.TrimSpace(rawPath) == "" {
			return governance.Result{}, errors.New("path is required")
		}
		oldString, ok := stringArg(args, "old_string")
		if !ok || oldString == "" {
			return governance.Result{}, errors.New("old_string is required")
		}
		newString, ok := stringArg(args, "new_string")
		if !ok {
			return governance.Result{}, errors.New("new_string must be a string")
		}
		if oldString == newString {
			return governance.Result{}, errors.New("new_string must differ from old_string")
		}
		replaceAll, err := boolArg(args, "replace_all", false)
		if err != nil {
			return governance.Result{}, err
		}
		abs, _, err := ws.Resolve(rawPath)
		if err != nil {
			return governance.Result{}, err
		}
		data, err := readRegularFile(abs)
		if err != nil {
			return governance.Result{}, errors.New("file not found")
		}
		content := string(data)
		count := strings.Count(content, oldString)
		if count == 0 {
			if fuzzy := fuzzyMatch(content, oldString); fuzzy != "" {
				return governance.Result{}, fmt.Errorf("old_string not found; closest match:\n%s", truncateRunes(fuzzy, 500))
			}
			return governance.Result{}, errors.New("old_string not found")
		}
		if count > 1 && !replaceAll {
			return governance.Result{}, fmt.Errorf("old_string matches %d locations; include more context or use replace_all", count)
		}
		var updated string
		if replaceAll {
			updated = strings.ReplaceAll(content, oldString, newString)
		} else {
			updated = strings.Replace(content, oldString, newString, 1)
		}
		if err := checkSyntax(abs, content, updated); err != nil {
			return governance.Result{}, err
		}
		if err := writeRegularFile(abs, updated); err != nil {
			return governance.Result{}, errors.New("file is unavailable")
		}
		ws.NoteWrite(requestID(ctx), abs, FileHash([]byte(updated)))
		diff := unifiedDiff(rawPath, oldString, newString, count, replaceAll)
		return governance.Result{Content: diff, Outcome: governance.OutcomeProductive}, nil
	}
}

func unifiedDiff(path, oldString, newString string, count int, replaceAll bool) string {
	mode := "1 occurrence replaced"
	if replaceAll {
		mode = fmt.Sprintf("%d occurrences replaced", count)
	}
	oldLines := strings.Split(oldString, "\n")
	newLines := strings.Split(newString, "\n")
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n@@ patch (%s) @@\n", path, path, mode)
	for _, line := range oldLines {
		sb.WriteString("-" + line + "\n")
	}
	for _, line := range newLines {
		sb.WriteString("+" + line + "\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// fuzzyMatch applies tolerant whitespace matching so minor indentation
// differences still locate the intended block.
func fuzzyMatch(content, needle string) string {
	norm := func(s string) string {
		return strings.Join(strings.Fields(s), " ")
	}
	want := norm(needle)
	if want == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	window := len(strings.Split(needle, "\n"))
	if window < 1 {
		window = 1
	}
	best := ""
	bestLen := 0
	for i := 0; i < len(lines); i++ {
		for size := 1; size <= window+2 && i+size <= len(lines); size++ {
			candidate := strings.Join(lines[i:i+size], "\n")
			if strings.Contains(norm(candidate), want) || strings.Contains(want, norm(candidate)) {
				if len(candidate) > bestLen {
					bestLen = len(candidate)
					best = candidate
				}
			}
		}
		// Cap work on large files.
		if i > 4000 {
			break
		}
	}
	return best
}
