package imessage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func TestClearStaleTypingIndicatorsClearsAllChats(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/server/info":
			_, _ = w.Write([]byte(`{"data":{"private_api":true,"helper_connected":true}}`))
		case r.URL.Path == "/api/v1/chat/query":
			_, _ = w.Write([]byte(`{"status":200,"message":"ok","data":[{"guid":"chat-a"},{"guid":"chat-b"},{"guid":"chat-a"}]}`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/typing") && r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, r.URL.EscapedPath())
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"guid":"ok"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"guid":"ok"}}`))
		}
	}))
	defer server.Close()
	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
	g.clearStaleTypingIndicators()
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 2 {
		t.Fatalf("expected 2 typing clears, got %v", deleted)
	}
}

func TestClearStaleTypingIndicatorsToleratesQueryFailure(t *testing.T) {
	var typingDeletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/server/info":
			_, _ = w.Write([]byte(`{"data":{"private_api":true,"helper_connected":true}}`))
		case r.URL.Path == "/api/v1/chat/query":
			http.Error(w, "failed", http.StatusBadGateway)
		case strings.HasSuffix(r.URL.EscapedPath(), "/typing"):
			typingDeletes++
			_, _ = w.Write([]byte(`{"data":{"guid":"ok"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"guid":"ok"}}`))
		}
	}))
	defer server.Close()
	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
	g.clearStaleTypingIndicators()
	if typingDeletes != 0 {
		t.Fatalf("expected no typing clears on query failure, got %d", typingDeletes)
	}
}

func TestQueryChatGUIDsDedupesAndTrims(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"guid": " a "}, {"guid": "a"}, {"guid": ""}, {"guid": "b"}}})
	}))
	defer server.Close()
	g := &Gateway{BlueBubblesURL: server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError)}
	guids, err := g.queryChatGUIDs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(guids) != 2 || guids[0] != "a" || guids[1] != "b" {
		t.Fatalf("unexpected guids %v", guids)
	}
}
