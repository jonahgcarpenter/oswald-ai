package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"gopkg.in/yaml.v3"
)

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

func boolArg(args map[string]interface{}, key string, def bool) (bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return def, nil
	}
	v, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return v, nil
}

// checkSyntax validates newContent for linted extensions, surfacing only
// errors the write introduces. Old content that already failed validation
// suppresses the same-class failure so pre-existing breakage is filtered.
func checkSyntax(path, oldContent, newContent string) error {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".json":
		oldOK := json.Valid([]byte(oldContent))
		if json.Valid([]byte(newContent)) {
			return nil
		}
		if !oldOK {
			return nil
		}
		var v interface{}
		if err := json.Unmarshal([]byte(newContent), &v); err != nil {
			return fmt.Errorf("json syntax error: %s", truncateRunes(err.Error(), 300))
		}
		return errors.New("json syntax error")
	case ".yaml", ".yml":
		var oldV interface{}
		oldErr := yaml.Unmarshal([]byte(oldContent), &oldV)
		var newV interface{}
		if err := yaml.Unmarshal([]byte(newContent), &newV); err != nil {
			if oldErr != nil || (strings.TrimSpace(oldContent) != "" && oldV == nil) {
				return nil
			}
			return fmt.Errorf("yaml syntax error: %s", truncateRunes(err.Error(), 300))
		}
		return nil
	case ".py":
		return checkPythonSyntax(oldContent, newContent)
	case ".toml":
		return nil
	default:
		return nil
	}
}

func checkPythonSyntax(oldContent, newContent string) error {
	newOK := pythonCompiles(newContent)
	if newOK {
		return nil
	}
	if !pythonCompiles(oldContent) && strings.TrimSpace(oldContent) != "" {
		return nil
	}
	// Re-run once to capture the message for the new content.
	msg := pythonCompileError(newContent)
	if msg == "" {
		return errors.New("python syntax error")
	}
	return fmt.Errorf("python syntax error: %s", truncateRunes(msg, 500))
}

func pythonCompiles(src string) bool {
	return pythonCompileError(src) == ""
}

func pythonCompileError(src string) string {
	python, err := exec.LookPath("python3")
	if err != nil {
		return ""
	}
	tmp, err := os.CreateTemp("", "oswald-py-*.py")
	if err != nil {
		return ""
	}
	name := tmp.Name()
	defer os.Remove(name)
	if !utf8.ValidString(src) {
		return "invalid UTF-8"
	}
	if _, err := tmp.WriteString(src); err != nil {
		tmp.Close()
		return ""
	}
	tmp.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-m", "py_compile", name)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return err.Error()
	}
	// Strip the temp filename so the model sees a stable message.
	msg = strings.ReplaceAll(msg, name, "<file>")
	return msg
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

func secondsToDuration(secs int) (d time.Duration) {
	return time.Duration(secs) * time.Second
}

func appendNote(existing, note string) string {
	if strings.TrimSpace(existing) == "" {
		return note
	}
	return existing + "\n" + note
}

func userHome() string {
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return home
	}
	return "/"
}
