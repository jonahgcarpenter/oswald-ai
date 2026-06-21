package sessionhistory

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

func TestRecentHandlerFormatsNewestFirstAndClampsCount(t *testing.T) {
	store := memory.NewStore(memory.Options{}, config.NewLogger(config.LevelError))
	for _, label := range []string{"one", "two", "three", "four"} {
		store.AppendTurn("s", llm.ChatMessage{Role: "user", Content: label}, llm.ChatMessage{Role: "assistant", Content: "reply " + label}, nil)
	}
	handler := NewRecentHandler(store, config.NewLogger(config.LevelError))
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: "req", SessionID: "s", SenderID: "u", Gateway: "websocket", Model: "m"})

	got, err := handler(ctx, map[string]interface{}{"offset": float64(1), "count": float64(99)})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !strings.Contains(got, "Exchange 1:\nUser: four") || !strings.Contains(got, "Exchange 2:\nUser: three") || !strings.Contains(got, "Exchange 3:\nUser: two") {
		t.Fatalf("unexpected recent output:\n%s", got)
	}
	if strings.Contains(got, "User: one") {
		t.Fatalf("expected count clamped to newest three, got:\n%s", got)
	}
}

func TestRecentHandlerRequiresSessionID(t *testing.T) {
	handler := NewRecentHandler(memory.NewStore(memory.Options{}, config.NewLogger(config.LevelError)), config.NewLogger(config.LevelError))
	if _, err := handler(context.Background(), nil); err == nil {
		t.Fatal("expected missing session error")
	}
}
