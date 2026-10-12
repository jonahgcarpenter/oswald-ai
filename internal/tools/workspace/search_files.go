package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// NewSearchFilesHandler returns content and filename search inside the workspace.
func NewSearchFilesHandler(ws *Workspace) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		if ws == nil || ws.Root() == "" {
			return governance.Result{}, errors.New("workspace is unavailable")
		}
		if _, ok := requestctx.PrincipalFromContext(ctx); !ok {
			return governance.Result{}, errors.New("workspace: authenticated principal required")
		}
		if err := rejectUnknown(args, "pattern", "target", "path", "file_glob", "limit", "offset", "order", "output_mode", "context"); err != nil {
			return governance.Result{}, err
		}
		pattern, ok := stringArg(args, "pattern")
		if !ok || strings.TrimSpace(pattern) == "" {
			return governance.Result{}, errors.New("pattern is required")
		}
		if len(pattern) > 1024 {
			return governance.Result{}, errors.New("pattern exceeds size limit")
		}
		target := "content"
		if raw, ok := stringArg(args, "target"); ok && strings.TrimSpace(raw) != "" {
			target = strings.TrimSpace(raw)
		}
		if target != "content" && target != "files" {
			return governance.Result{}, errors.New("target must be content or files")
		}
		base := "."
		if raw, ok := stringArg(args, "path"); ok && strings.TrimSpace(raw) != "" {
			base = raw
		}
		fileGlob, _ := stringArg(args, "file_glob")
		limit, err := intArg(args, "limit", 50)
		if err != nil || limit < 1 || limit > 500 {
			return governance.Result{}, errors.New("limit must be an integer between 1 and 500")
		}
		offset, err := intArg(args, "offset", 0)
		if err != nil || offset < 0 {
			return governance.Result{}, errors.New("offset must be an integer >= 0")
		}
		order := "discovery"
		if raw, ok := stringArg(args, "order"); ok && strings.TrimSpace(raw) != "" {
			order = strings.TrimSpace(raw)
		}
		if order != "discovery" && order != "modified" {
			return governance.Result{}, errors.New("order must be discovery or modified")
		}
		outputMode := "content"
		if raw, ok := stringArg(args, "output_mode"); ok && strings.TrimSpace(raw) != "" {
			outputMode = strings.TrimSpace(raw)
		}
		if outputMode != "content" && outputMode != "files_only" && outputMode != "count" {
			return governance.Result{}, errors.New("output_mode must be content, files_only, or count")
		}
		contextLines, err := intArg(args, "context", 0)
		if err != nil || contextLines < 0 || contextLines > 10 {
			return governance.Result{}, errors.New("context must be an integer between 0 and 10")
		}
		searchRoot, err := ws.ResolveDir(base)
		if err != nil {
			return governance.Result{}, err
		}
		// A direct file path searches just that file.
		if info, err := os.Lstat(searchRoot); err == nil && !info.IsDir() {
			return searchSingleFile(ctx, ws, searchRoot, pattern, outputMode, contextLines)
		}
		if target == "files" {
			return searchFilenames(ctx, ws, searchRoot, pattern, order, limit, offset)
		}
		return searchContents(ctx, ws, searchRoot, pattern, fileGlob, outputMode, contextLines, limit, offset)
	}
}

func searchSingleFile(ctx context.Context, ws *Workspace, abs, pattern, outputMode string, contextLines int) (governance.Result, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return governance.Result{}, errors.New("pattern is not a valid regex")
	}
	data, err := readRegularFile(abs)
	if err != nil {
		return governance.Result{}, errors.New("file is unavailable")
	}
	rel, _ := filepath.Rel(ws.Root(), abs)
	matches := matchLines(string(data), re, contextLines)
	switch outputMode {
	case "files_only":
		if len(matches) == 0 {
			return governance.Result{Content: "(no matches)", Outcome: governance.OutcomeUnproductive}, nil
		}
		return governance.Result{Content: rel, Outcome: governance.OutcomeProductive}, nil
	case "count":
		return governance.Result{Content: fmt.Sprintf("%s: %d", rel, len(matches)), Outcome: resultOutcome(len(matches) > 0)}, nil
	default:
		if len(matches) == 0 {
			return governance.Result{Content: "(no matches)", Outcome: governance.OutcomeUnproductive}, nil
		}
		var sb strings.Builder
		for _, m := range matches {
			fmt.Fprintf(&sb, "%s:%s\n", rel, m)
		}
		return governance.Result{Content: strings.TrimSuffix(sb.String(), "\n"), Outcome: governance.OutcomeProductive}, nil
	}
}

func searchFilenames(ctx context.Context, ws *Workspace, root, glob, order string, limit, offset int) (governance.Result, error) {
	type entry struct {
		rel     string
		modTime int64
	}
	var found []entry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" || name == "__pycache__" {
				return filepath.SkipDir
			}
			if _, _, derr := ws.Resolve(path); derr != nil {
				return filepath.SkipDir
			}
			return nil
		}
		if _, _, derr := ws.Resolve(path); derr != nil {
			return nil
		}
		matched, merr := filepath.Match(glob, name)
		if merr != nil {
			// Fall back to substring on invalid glob.
			matched = strings.Contains(strings.ToLower(name), strings.ToLower(glob))
		} else if glob == "" {
			matched = true
		}
		// Bare patterns without wildcards behave as substring filters.
		if !strings.ContainsAny(glob, "*?[") && glob != "" {
			matched = strings.Contains(strings.ToLower(name), strings.ToLower(glob))
		}
		if !matched {
			return nil
		}
		rel, _ := filepath.Rel(ws.Root(), path)
		var mod int64
		if info, err := d.Info(); err == nil {
			mod = info.ModTime().UnixNano()
		}
		found = append(found, entry{rel: rel, modTime: mod})
		if len(found) > 5000 {
			return errors.New("search exceeds result cap")
		}
		return nil
	})
	if err != nil {
		return governance.Result{}, errors.New("search failed")
	}
	if order == "modified" {
		sort.Slice(found, func(i, j int) bool { return found[i].modTime > found[j].modTime })
	} else {
		sort.Slice(found, func(i, j int) bool { return found[i].rel < found[j].rel })
	}
	if offset >= len(found) {
		return governance.Result{Content: "(no matches)", Outcome: governance.OutcomeUnproductive}, nil
	}
	end := offset + limit
	if end > len(found) {
		end = len(found)
	}
	var sb strings.Builder
	for _, e := range found[offset:end] {
		sb.WriteString(e.rel + "\n")
	}
	if len(found[offset:end]) == 0 {
		return governance.Result{Content: "(no matches)", Outcome: governance.OutcomeUnproductive}, nil
	}
	return governance.Result{Content: strings.TrimSuffix(sb.String(), "\n"), Outcome: governance.OutcomeProductive}, nil
}

func searchContents(ctx context.Context, ws *Workspace, root, pattern, fileGlob, outputMode string, contextLines, limit, offset int) (governance.Result, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return governance.Result{}, errors.New("pattern is not a valid regex")
	}
	type fileResult struct {
		rel     string
		matches []string
	}
	var files []fileResult
	skipped := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "__pycache__" {
				return filepath.SkipDir
			}
			if _, _, derr := ws.Resolve(path); derr != nil {
				return filepath.SkipDir
			}
			return nil
		}
		if fileGlob != "" {
			if ok, _ := filepath.Match(fileGlob, d.Name()); !ok {
				return nil
			}
		}
		if _, _, derr := ws.Resolve(path); derr != nil {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > 1<<20 {
			skipped++
			return nil
		}
		data, rerr := readRegularFile(path)
		if rerr != nil {
			return nil
		}
		matches := matchLines(string(data), re, contextLines)
		if len(matches) == 0 {
			return nil
		}
		rel, _ := filepath.Rel(ws.Root(), path)
		files = append(files, fileResult{rel: rel, matches: matches})
		if len(files) > 500 {
			return errors.New("search exceeds result cap")
		}
		return nil
	})
	if err != nil {
		return governance.Result{}, errors.New("search failed")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	switch outputMode {
	case "files_only":
		rels := make([]string, 0, len(files))
		for _, f := range files {
			rels = append(rels, f.rel)
		}
		return paginateLines(rels, limit, offset)
	case "count":
		lines := make([]string, 0, len(files))
		for _, f := range files {
			lines = append(lines, fmt.Sprintf("%s: %d", f.rel, len(f.matches)))
		}
		return paginateLines(lines, limit, offset)
	default:
		var lines []string
		for _, f := range files {
			for _, m := range f.matches {
				lines = append(lines, fmt.Sprintf("%s:%s", f.rel, m))
				if len(lines) > 2000 {
					break
				}
			}
		}
		_ = skipped
		return paginateLines(lines, limit, offset)
	}
}

func matchLines(content string, re *regexp.Regexp, contextLines int) []string {
	lines := strings.Split(content, "\n")
	var out []string
	for i, line := range lines {
		// Skip NUL-bearing lines (binary).
		if strings.IndexByte(line, 0) >= 0 {
			continue
		}
		loc := re.FindStringIndex(line)
		if loc == nil {
			continue
		}
		if contextLines == 0 {
			out = append(out, fmt.Sprintf("%d:%s", i+1, truncateRunes(line, 500)))
			continue
		}
		start := i - contextLines
		if start < 0 {
			start = 0
		}
		end := i + contextLines
		if end >= len(lines) {
			end = len(lines) - 1
		}
		for j := start; j <= end; j++ {
			prefix := " "
			if j == i {
				prefix = ">"
			}
			out = append(out, fmt.Sprintf("%s%d:%s", prefix, j+1, truncateRunes(lines[j], 500)))
		}
		out = append(out, "--")
		if len(out) > 200 {
			break
		}
	}
	return out
}

func paginateLines(lines []string, limit, offset int) (governance.Result, error) {
	if offset >= len(lines) || len(lines) == 0 {
		return governance.Result{Content: "(no matches)", Outcome: governance.OutcomeUnproductive}, nil
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}
	return governance.Result{Content: strings.Join(lines[offset:end], "\n"), Outcome: governance.OutcomeProductive}, nil
}

func resultOutcome(found bool) governance.Outcome {
	if found {
		return governance.OutcomeProductive
	}
	return governance.OutcomeUnproductive
}
