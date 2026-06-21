package sessionhistory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestSummaryHandlerReturnsSummary(t *testing.T) {
	store := usermemory.NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	if err := store.UpsertSessionSummary(usermemory.SessionSummary{SessionID: "session-1", UserID: "usr_test", Summary: "Working on memory."}); err != nil {
		t.Fatal(err)
	}
	handler := NewSummaryHandler(store, config.NewLogger(config.LevelError))
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{SessionID: "session-1"})
	result, err := handler(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Working on memory") {
		t.Fatalf("unexpected result: %s", result)
	}
}
