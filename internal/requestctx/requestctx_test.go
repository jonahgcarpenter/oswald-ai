package requestctx

import (
	"context"
	"testing"
)

type testExposer struct{ names []string }

func (e *testExposer) ExposeTools(names []string) { e.names = append(e.names, names...) }

func TestMetadataFallsBackToSenderID(t *testing.T) {
	ctx := WithSenderID(context.Background(), "sender-1")
	ctx = WithMetadata(ctx, Metadata{RequestID: "req-1", SessionID: "session-1"})

	meta := MetadataFromContext(ctx)
	if meta.SenderID != "sender-1" || meta.RequestID != "req-1" || meta.SessionID != "session-1" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
}

func TestToolExposerRoundTrip(t *testing.T) {
	exposer := &testExposer{}
	ctx := WithToolExposer(context.Background(), exposer)
	got := ToolExposerFromContext(ctx)
	if got == nil {
		t.Fatal("expected exposer")
	}
	got.ExposeTools([]string{"tool"})
	if len(exposer.names) != 1 || exposer.names[0] != "tool" {
		t.Fatalf("unexpected exposer calls: %+v", exposer.names)
	}
}
