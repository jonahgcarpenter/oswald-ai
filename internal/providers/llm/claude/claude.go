// Package claude owns the Claude subscription transport through the official
// Agent SDK path: the unmodified `claude` CLI on the host owns the login and
// refresh lifecycle. Oswald never reads, copies, or refreshes native tokens;
// it gates on `claude auth status --json` and spawns one request-scoped child
// per turn with subscription credentials inherited from the CLI session.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

const (
	statusTimeout = 10 * time.Second
	chatTimeout   = 5 * time.Minute
)

// Status is the parsed `claude auth status --json` result.
type Status struct {
	LoggedIn bool
	Plan     string
}

var (
	lookPath   = exec.LookPath
	runCommand = func(ctx context.Context, name string, args []string, env []string, stdin string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = env
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var out, errOut bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errOut
		if err := cmd.Run(); err != nil {
			return out.String(), err
		}
		return out.String(), nil
	}
)

// BinaryPath resolves the installed CLI or returns an error.
func BinaryPath() (string, error) {
	path, err := lookPath("claude")
	if err != nil || strings.TrimSpace(path) == "" {
		return "", errors.New("claude CLI is not installed")
	}
	return path, nil
}

// Check verifies the CLI exists and reports a logged-in subscription.
func Check(ctx context.Context) (Status, error) {
	bin, err := BinaryPath()
	if err != nil {
		return Status{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	out, err := runCommand(ctx, bin, []string{"auth", "status", "--json"}, nil, "")
	if err != nil && strings.TrimSpace(out) == "" {
		return Status{}, errors.New("claude CLI is not logged in")
	}
	var payload struct {
		LoggedIn *bool  `json:"loggedIn"`
		Plan     string `json:"plan"`
	}
	if jerr := json.Unmarshal([]byte(out), &payload); jerr != nil {
		if err != nil {
			return Status{}, errors.New("claude CLI is not logged in")
		}
		return Status{LoggedIn: true}, nil
	}
	if payload.LoggedIn != nil && !*payload.LoggedIn {
		return Status{}, errors.New("claude CLI is not logged in")
	}
	return Status{LoggedIn: true, Plan: strings.TrimSpace(payload.Plan)}, nil
}

// StaticModels is the subscription catalog used for /model listings. Windows
// are the public Claude defaults; live listing stays with the CLI.
func StaticModels() []llm.ModelEntry {
	return []llm.ModelEntry{
		{ID: "claude-sonnet-4-6", Window: 200000},
		{ID: "claude-opus-4-6", Window: 200000},
		{ID: "claude-haiku-4-5", Window: 200000},
	}
}

// Client is a request-scoped Claude subscription model client. It carries no
// credential and performs no token I/O; every Chat spawns the official CLI.
type Client struct {
	binary string
}

// NewClient verifies the CLI exists and returns a subscription client.
func NewClient() (*Client, error) {
	bin, err := BinaryPath()
	if err != nil {
		return nil, err
	}
	return &Client{binary: bin}, nil
}

// promptText renders messages as plain text for the CLI child. Tool
// definitions ride as a JSON preamble so the model sees the available tools;
// tool execution stays in Oswald's loop.
func promptText(req llm.ChatRequest) string {
	var b strings.Builder
	if len(req.Tools) > 0 {
		names := make([]string, 0, len(req.Tools))
		for _, t := range req.Tools {
			names = append(names, t.Function.Name)
		}
		fmt.Fprintf(&b, "Available tools: %s\n\n", strings.Join(names, ", "))
	}
	for _, m := range req.Messages {
		role := strings.TrimSpace(m.Role)
		if role == "" {
			role = "user"
		}
		text := strings.TrimSpace(m.Content)
		if text == "" && len(m.ToolCalls) > 0 {
			parts := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				parts = append(parts, tc.Function.Name+" "+strings.TrimSpace(tc.Function.RawArguments))
			}
			text = "tool_calls: " + strings.Join(parts, "; ")
		}
		if m.ToolCallID != "" && text != "" {
			fmt.Fprintf(&b, "%s [%s]: %s\n", role, m.ToolCallID, text)
			continue
		}
		if text != "" {
			fmt.Fprintf(&b, "%s: %s\n", role, text)
		}
	}
	return b.String()
}

// cleanEnv drops credential and override variables the CLI child must not
// inherit so a key Oswald holds for its own API provider never forces the
// subscription session onto API billing.
func cleanEnv() []string {
	drop := map[string]bool{
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
		"ANTHROPIC_FOUNDRY_API_KEY": true, "ANTHROPIC_BASE_URL": true,
		"ANTHROPIC_API_URL": true, "CLAUDE_CODE_USE_BEDROCK": true,
		"CLAUDE_CODE_USE_VERTEX": true, "CLAUDE_CODE_USE_FOUNDRY": true,
		"CLAUDE_CODE_ENTRYPOINT": true,
	}
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.Index(kv, "="); i >= 0 {
			name = kv[:i]
		}
		if drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Chat implements llm.Chatter via one request-scoped `claude -p` child.
func (c *Client) Chat(ctx context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, errors.New("claude subscription requires a model")
	}
	if _, err := Check(ctx); err != nil {
		return nil, err
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()
	args := []string{"-p", "--output-format", "stream-json", "--model", model}
	out, err := runCommand(ctx, c.binary, args, cleanEnv(), promptText(req))
	if err != nil {
		return nil, errors.New("claude subscription request failed")
	}
	text := parseStreamJSON(out)
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("claude subscription returned no text")
	}
	if cb != nil {
		cb(llm.ChatMessage{Role: "assistant", Content: text})
	}
	return &llm.ChatResponse{Model: model, Message: llm.ChatMessage{Role: "assistant", Content: text}, DurationMS: time.Since(started).Milliseconds(), DoneReason: "stop"}, nil
}

// parseStreamJSON extracts assistant text from `claude -p --output-format
// stream-json` output. Unknown lines are ignored; the final result envelope
// wins when present.
func parseStreamJSON(out string) string {
	var b strings.Builder
	var result string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var evt map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(evt["type"], &typ)
		if typ == "result" {
			var payload struct {
				Result string `json:"result"`
			}
			if err := json.Unmarshal([]byte(line), &payload); err == nil && strings.TrimSpace(payload.Result) != "" {
				result = payload.Result
			}
			continue
		}
		var msg struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		for _, part := range msg.Message.Content {
			if part.Type == "text" && part.Text != "" {
				b.WriteString(part.Text)
			}
		}
	}
	if strings.TrimSpace(result) != "" {
		return result
	}
	return b.String()
}

// ExecutableDir returns the CLI directory for diagnostics without values.
func ExecutableDir() string {
	bin, err := BinaryPath()
	if err != nil {
		return ""
	}
	return filepath.Dir(bin)
}
