package browser

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	providerbrowser "github.com/jonahgcarpenter/oswald-ai/internal/providers/browser"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
)

type fakeBackend struct {
	navigate providerbrowser.NavigateData
	snapshot providerbrowser.SnapshotData
	console  providerbrowser.ConsoleData
	eval     providerbrowser.EvalData
	images   providerbrowser.ImagesData
	shot     providerbrowser.ScreenshotData
	err      error
}

func (f *fakeBackend) Navigate(_ context.Context, _, _ string) (providerbrowser.NavigateData, error) {
	return f.navigate, f.err
}
func (f *fakeBackend) Snapshot(_ context.Context, _ string, _ bool) (providerbrowser.SnapshotData, error) {
	return f.snapshot, f.err
}
func (f *fakeBackend) Click(_ context.Context, _, _ string) (providerbrowser.SnapshotData, error) {
	return f.snapshot, f.err
}
func (f *fakeBackend) Fill(_ context.Context, _, _, _ string) (providerbrowser.SnapshotData, error) {
	return f.snapshot, f.err
}
func (f *fakeBackend) Scroll(_ context.Context, _, _ string) (providerbrowser.SnapshotData, error) {
	return f.snapshot, f.err
}
func (f *fakeBackend) Back(_ context.Context, _ string) (providerbrowser.NavigateData, error) {
	return f.navigate, f.err
}
func (f *fakeBackend) Press(_ context.Context, _, _ string) (providerbrowser.SnapshotData, error) {
	return f.snapshot, f.err
}
func (f *fakeBackend) Console(_ context.Context, _ string, _ bool) (providerbrowser.ConsoleData, error) {
	return f.console, f.err
}
func (f *fakeBackend) Eval(_ context.Context, _, _ string) (providerbrowser.EvalData, error) {
	return f.eval, f.err
}
func (f *fakeBackend) Images(_ context.Context, _ string) (providerbrowser.ImagesData, error) {
	return f.images, f.err
}
func (f *fakeBackend) Screenshot(_ context.Context, _ string, _ bool) (providerbrowser.ScreenshotData, error) {
	return f.shot, f.err
}
func (f *fakeBackend) RespondToDialog(_ context.Context, _, _, _, _ string) (providerbrowser.SnapshotData, error) {
	return f.snapshot, f.err
}

func testContext() context.Context {
	ctx := context.Background()
	ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway})
	return ctx
}

func testDeps(t *testing.T, backend providerbrowser.Backend) Deps {
	t.Helper()
	return Deps{Backend: backend, Cache: imagecache.New(t.TempDir()), Log: config.NewLogger(config.LevelError)}
}

func TestNavigateHandlerEnvelope(t *testing.T) {
	backend := &fakeBackend{navigate: providerbrowser.NavigateData{URL: "https://example.com", Title: "Example", Snapshot: "[@e1] link", ElementCount: 1}}
	handlers := NewHandlers(testDeps(t, backend))
	result, err := handlers[NavigateName](testContext(), map[string]interface{}{"url": "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "<untrusted_tool_result") || !strings.Contains(result.Content, "@e1") {
		t.Fatalf("content = %q", result.Content)
	}
	if _, err := handlers[NavigateName](testContext(), map[string]interface{}{}); err == nil {
		t.Fatal("missing url succeeded")
	}
	if _, err := handlers[NavigateName](testContext(), map[string]interface{}{"url": "https://example.com", "extra": 1}); err == nil {
		t.Fatal("unknown arg succeeded")
	}
	if _, err := handlers[NavigateName](context.Background(), map[string]interface{}{"url": "https://example.com"}); err == nil {
		t.Fatal("unauthenticated succeeded")
	}
}

func TestSnapshotClickScrollPressBackConsoleImages(t *testing.T) {
	snap := providerbrowser.SnapshotData{Snapshot: "[@e1] link", ElementCount: 1}
	backend := &fakeBackend{
		navigate: providerbrowser.NavigateData{URL: "https://example.com", Snapshot: "[@e1] link", ElementCount: 1},
		snapshot: snap,
		console:  providerbrowser.ConsoleData{Messages: []providerbrowser.ConsoleMessage{}, Errors: []providerbrowser.JSError{}},
		eval:     providerbrowser.EvalData{Result: "1"},
		images:   providerbrowser.ImagesData{Images: []providerbrowser.ImageInfo{}, Count: 0},
	}
	handlers := NewHandlers(testDeps(t, backend))
	ctx := testContext()
	cases := []struct {
		tool string
		args map[string]interface{}
	}{
		{SnapshotName, map[string]interface{}{}},
		{SnapshotName, map[string]interface{}{"full": true}},
		{ClickName, map[string]interface{}{"ref": "@e1"}},
		{TypeName, map[string]interface{}{"ref": "@e1", "text": "hello"}},
		{ScrollName, map[string]interface{}{"direction": "down"}},
		{BackName, map[string]interface{}{}},
		{PressName, map[string]interface{}{"key": "Enter"}},
		{ConsoleName, map[string]interface{}{}},
		{ConsoleName, map[string]interface{}{"expression": "document.title"}},
		{GetImagesName, map[string]interface{}{}},
	}
	for _, tc := range cases {
		result, err := handlers[tc.tool](ctx, tc.args)
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		if !strings.Contains(result.Content, "<untrusted_tool_result") {
			t.Fatalf("%s content missing wrapper: %q", tc.tool, result.Content)
		}
	}
	if _, err := handlers[ScrollName](ctx, map[string]interface{}{"direction": "sideways"}); err == nil {
		t.Fatal("bad direction succeeded")
	}
	if _, err := handlers[ConsoleName](ctx, map[string]interface{}{"expression": ""}); err == nil {
		t.Fatal("empty expression succeeded")
	}
}

func TestVisionHandlerAttachesImage(t *testing.T) {
	backend := &fakeBackend{shot: providerbrowser.ScreenshotData{PNG: testPNG()}}
	handlers := NewHandlers(testDeps(t, backend))
	result, err := handlers[VisionName](testContext(), map[string]interface{}{"question": "what do you see?"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 || !strings.Contains(result.Content, "screenshot_path:") {
		t.Fatalf("vision result = %+v", result.Content)
	}
	if _, err := handlers[VisionName](testContext(), map[string]interface{}{}); err == nil {
		t.Fatal("missing question succeeded")
	}
}

func testPNG() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestNavigateBotDetectionWarning(t *testing.T) {
	backend := &fakeBackend{navigate: providerbrowser.NavigateData{URL: "https://example.com", Title: "Just a moment - captcha", Snapshot: "[@e1] link", ElementCount: 1, BotWarning: true}}
	handlers := NewHandlers(testDeps(t, backend))
	result, err := handlers[NavigateName](testContext(), map[string]interface{}{"url": "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"bot_detection_warning"`) {
		t.Fatalf("bot warning missing: %q", result.Content)
	}
	if strings.Contains(result.Content, `"bot_warning"`) {
		t.Fatalf("legacy bot_warning key present: %q", result.Content)
	}
	plain := &fakeBackend{navigate: providerbrowser.NavigateData{URL: "https://example.com", Title: "Example", Snapshot: "[@e1] link", ElementCount: 1}}
	handlers = NewHandlers(testDeps(t, plain))
	result, err = handlers[NavigateName](testContext(), map[string]interface{}{"url": "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Content, "bot_detection_warning") {
		t.Fatalf("unexpected bot warning: %q", result.Content)
	}
}

func TestDialogHandlerResolvesAndRefreshes(t *testing.T) {
	snap := providerbrowser.SnapshotData{Snapshot: "[@e1] link", ElementCount: 1}
	backend := &fakeBackend{snapshot: snap}
	handlers := NewHandlers(testDeps(t, backend))
	result, err := handlers[DialogName](testContext(), map[string]interface{}{"action": "accept"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "<untrusted_tool_result") {
		t.Fatalf("dialog content missing wrapper: %q", result.Content)
	}
	if _, err := handlers[DialogName](testContext(), map[string]interface{}{"action": "maybe"}); err == nil {
		t.Fatal("bad dialog action succeeded")
	}
	if _, err := handlers[DialogName](testContext(), map[string]interface{}{}); err == nil {
		t.Fatal("missing dialog action succeeded")
	}
	pending := &fakeBackend{snapshot: providerbrowser.SnapshotData{
		Snapshot: "[@e1] link", ElementCount: 1,
		PendingDialogs: []providerbrowser.PendingDialog{{ID: "1", Type: "alert", Message: "Are you sure?"}},
	}}
	handlers = NewHandlers(testDeps(t, pending))
	result, err = handlers[DialogName](testContext(), map[string]interface{}{"action": "dismiss"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"pending_dialogs"`) || !strings.Contains(result.Content, "Are you sure?") {
		t.Fatalf("pending dialogs missing from result: %q", result.Content)
	}
}

func TestPartialNavigateMarksDegraded(t *testing.T) {
	backend := &fakeBackend{navigate: providerbrowser.NavigateData{URL: "https://example.com/slow", Title: "Slow", Snapshot: "[@e1] link", ElementCount: 1, Partial: true}}
	handlers := NewHandlers(testDeps(t, backend))
	result, err := handlers[NavigateName](testContext(), map[string]interface{}{"url": "https://example.com/slow"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsDegraded || result.ReasonCode != "partial_load" {
		t.Fatalf("partial result = %+v", result)
	}
	if !strings.Contains(result.Content, `"partial": true`) {
		t.Fatalf("partial flag missing: %q", result.Content)
	}
}
