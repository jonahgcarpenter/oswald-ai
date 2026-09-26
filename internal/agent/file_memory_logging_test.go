package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/files"
)

func TestProcessFileMemoryLoadTelemetry(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{
		{Message: llm.ChatMessage{Role: "assistant", Content: "first"}},
		{Message: llm.ChatMessage{Role: "assistant", Content: "second"}},
	}}
	a, _ := newTestAgent(t, chat, nil, nil)
	fileStore := files.NewStore(t.TempDir())
	a.SetFileMemory(fileStore)
	userContent := "private-user-π@example.test"
	memoryContent := "private-memory-雪"
	for _, entry := range []struct{ target, content string }{{"user", userContent}, {"memory", memoryContent}} {
		if _, err := fileStore.Apply(context.Background(), "user-1", entry.target, []files.Operation{{Action: "add", Content: entry.content}}); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&output)
	a.log = log
	for _, requestID := range []string{"first-request", "second-request"} {
		if _, err := processAgent(a, requestID, "homeassistant", "session", "user-1", "private-display@example.test", "private-prompt", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(chat.requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(chat.requests))
	}
	for _, canary := range []string{userContent, memoryContent, "private-display@example.test", "private-prompt"} {
		if strings.Contains(output.String(), canary) {
			t.Fatalf("log leaked private canary %q", canary)
		}
	}
	counts := map[string]int{}
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["event"] != "agent.memory.files.loaded" {
			continue
		}
		requestID, ok := record["request_id"].(string)
		if !ok {
			t.Fatalf("missing request correlation: %v", record)
		}
		counts[requestID]++
		if record["level"] != "info" || record["record_kind"] != "measurement" || record["status"] != "ok" || record["user_chars"] != float64(len([]rune(userContent))) || record["memory_chars"] != float64(len([]rune(memoryContent))) {
			t.Fatalf("invalid file load measurement: %v", record)
		}
		if duration, ok := record["duration_ms"].(float64); !ok || duration < 0 {
			t.Fatalf("invalid load duration: %v", record)
		}
	}
	if counts["first-request"] != 1 || counts["second-request"] != 1 || len(counts) != 2 {
		t.Fatalf("file load measurements per request = %v", counts)
	}
}

func TestProcessFileMemoryReadFailureWarnsWithoutModelSubmission(t *testing.T) {
	chat := &fakeChatter{}
	a, _ := newTestAgent(t, chat, nil, nil)
	root := t.TempDir()
	fileStore := files.NewStore(root)
	a.SetFileMemory(fileStore)
	if _, err := fileStore.Apply(context.Background(), "user-1", "user", []files.Operation{{Action: "add", Content: "private-user-canary"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("private-target-canary", filepath.Join(root, "user-1", "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&output)
	a.log = log
	response, err := a.Process(context.Background(), Request{
		RequestID:  "failed-read",
		Principal:  identity.Principal{CanonicalUserID: "user-1", ExternalID: "private-external-canary", Gateway: "homeassistant", Assurance: identity.AssuranceHomeAssistantToken},
		SessionKey: "private-session-canary", Prompt: "private-prompt-canary",
	})
	if err == nil || response != nil || !strings.Contains(err.Error(), "read file memory") {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if len(chat.requests) != 0 {
		t.Fatalf("model received %d requests after file read failure", len(chat.requests))
	}
	if strings.Contains(output.String(), "private-") {
		t.Fatal("private canary leaked into log")
	}
	loaded, failed := 0, 0
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		switch record["event"] {
		case "agent.memory.files.loaded":
			loaded++
		case "agent.memory.files.load_failed":
			failed++
			if record["level"] != "warn" || record["status"] != "error" || record["request_id"] != "failed-read" || record["error_code"] == nil {
				t.Fatalf("invalid file read warning: %v", record)
			}
			if duration, ok := record["duration_ms"].(float64); !ok || duration < 0 {
				t.Fatalf("invalid failure duration: %v", record)
			}
		}
	}
	if loaded != 0 || failed != 1 {
		t.Fatalf("loaded=%d load_failed=%d, want 0 and 1", loaded, failed)
	}
}
