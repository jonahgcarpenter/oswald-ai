package usermemory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

type fakeMemoryEmbedder struct{}

func (fakeMemoryEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	if strings.Contains(req.Input, "purple") {
		return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
	}
	return &llm.EmbedResponse{Embeddings: [][]float64{{0, 1}}}, nil
}

type countingMemoryEmbedder struct {
	inputs []string
}

func (f *countingMemoryEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	f.inputs = append(f.inputs, req.Input)
	if strings.Contains(req.Input, "purple") {
		return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
	}
	return &llm.EmbedResponse{Embeddings: [][]float64{{0, 1}}}, nil
}

func TestStoreSaveSearchAndForgetMemory(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck

	if err := store.SyncSpeakerIntro("usr_test", "You are speaking with Test User."); err != nil {
		t.Fatal(err)
	}
	entry, err := store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Scope != ScopeLongTerm || entry.Category != "durable_preferences" {
		t.Fatalf("unexpected entry: %+v", entry)
	}

	entries, err := store.Search(context.Background(), "usr_test", ScopeLongTerm, "", "purple", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.Contains(entries[0].Statement, "purple") {
		t.Fatalf("expected purple memory, got %+v", entries)
	}

	deleted, err := store.Forget("usr_test", "The user likes purple.", ScopeLongTerm)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("expected one deleted row, got %d", deleted)
	}
	entries, err = store.ListMemories("usr_test", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no active memories, got %+v", entries)
	}
}

func TestStoreShortTermExpiry(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck

	_, err := store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeShortTerm, Category: "tasks", Statement: "The user is testing expiry.", Evidence: "test", TTL: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	entries, err := store.ListMemories("usr_test", ScopeShortTerm, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected expired memory to be hidden, got %+v", entries)
	}
}

func TestStoreSessionContextIncludesSummaryAndRecentTurn(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fakeMemoryEmbedder{}, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck

	if err := store.AppendSessionTurn(context.Background(), "session-1", "usr_test", "I like purple", "Noted.", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}

	ctx, err := store.BuildContext(context.Background(), "usr_test", "session-1", "purple memory", ContextOptions{RecentTurns: 2, ContextBudgetChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx.Block, "Stable User Profile") || strings.Contains(ctx.Block, "Current Session Summary") || !strings.Contains(ctx.Block, "Recent Exchanges") {
		t.Fatalf("unexpected context block:\n%s", ctx.Block)
	}
}

func TestBuildContextDoesNotEmbedQuery(t *testing.T) {
	embedder := &countingMemoryEmbedder{}
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), embedder, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck

	if err := store.AppendSessionTurn(context.Background(), "session-1", "usr_test", "I like purple", "Noted.", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	seedEmbeddingCount := len(embedder.inputs)

	_, err = store.BuildContext(context.Background(), "usr_test", "session-1", "purple memory", ContextOptions{RecentTurns: 1, ContextBudgetChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	queryEmbeddings := embedder.inputs[seedEmbeddingCount:]
	if len(queryEmbeddings) != 0 {
		t.Fatalf("expected no query embeddings from automatic context, got %d: %+v", len(queryEmbeddings), queryEmbeddings)
	}
}
