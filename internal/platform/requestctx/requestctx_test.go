package requestctx

import (
	"context"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"testing"
)

type testExposer struct{ names []string }

func (e *testExposer) ExposeTools(names []string) { e.names = append(e.names, names...) }
func TestPrincipalAndMetadataRoundTrip(t *testing.T) {
	p := identity.Principal{CanonicalUserID: "sender-1", Gateway: "imessage", ExternalID: "external-1", Assurance: identity.AssuranceBlueBubblesWebhook}
	ctx := WithMetadata(WithPrincipal(context.Background(), p), Metadata{RequestID: "req-1", SessionID: "session-1", SessionGeneration: 3})
	meta := MetadataFromContext(ctx)
	got, ok := PrincipalFromContext(ctx)
	if !ok || got != p || meta.RequestID != "req-1" || meta.SessionID != "session-1" || meta.SessionGeneration != 3 {
		t.Fatal("request metadata changed")
	}
}
func TestToolExposerRoundTrip(t *testing.T) {
	e := &testExposer{}
	got := ToolExposerFromContext(WithToolExposer(context.Background(), e))
	if got == nil {
		t.Fatal("missing exposer")
	}
	got.ExposeTools([]string{"tool"})
	if len(e.names) != 1 || e.names[0] != "tool" {
		t.Fatal("discovery collector changed")
	}
}
func TestInputImagesUseDefensiveCopies(t *testing.T) {
	images := []InputImage{{MIMEType: "image/png", Data: "encoded", Source: "source"}}
	ctx := WithInputImages(context.Background(), images)
	images[0].Data = "mutated-input"
	got := InputImagesFromContext(ctx)
	if len(got) != 1 || got[0].Data != "encoded" {
		t.Fatal("caller mutated context images")
	}
	got[0].Data = "mutated-output"
	if InputImagesFromContext(ctx)[0].Data != "encoded" {
		t.Fatal("returned catalog shares context storage")
	}
}
func TestParentModelRoundTrip(t *testing.T) {
	ctx := WithParentModel(context.Background(), "  pinned-model  ", " alt-provider ")
	got, ok := ParentModelFromContext(ctx)
	if !ok || got.Model != "pinned-model" || got.Provider != "alt-provider" {
		t.Fatalf("parent model = %+v ok=%v", got, ok)
	}
}
func TestParentModelAbsentWhenEmpty(t *testing.T) {
	if _, ok := ParentModelFromContext(context.Background()); ok {
		t.Fatal("unsignaled context reported a parent model")
	}
	for _, tc := range []struct{ model, provider string }{
		{"", ""},
		{"   ", "   "},
		{"pinned-model", "  "},
		{"  ", "alt-provider"},
	} {
		ctx := WithParentModel(context.Background(), tc.model, tc.provider)
		if _, ok := ParentModelFromContext(ctx); ok {
			t.Fatalf("empty parent model %q/%q was attached", tc.model, tc.provider)
		}
	}
}
