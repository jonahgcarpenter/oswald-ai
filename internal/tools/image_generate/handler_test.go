package image_generate

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/image_generate/comfy_ui"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

type fakeGenerator struct {
	called           int
	workflow         *comfy_ui.Workflow
	input            []byte
	negative, aspect string
	strength         *float64
	result           comfy_ui.Generation
}

type fakeSourceCache struct {
	data []byte
	path string
	user string
	url  string
}

func (f *fakeSourceCache) ImportHTTPS(_ context.Context, user, url string) (string, []byte, string, error) {
	f.user, f.url = user, url
	return f.path, f.data, "image/png", nil
}

func (f *fakeSourceCache) Resolve(_ context.Context, user, path string) ([]byte, string, error) {
	f.user, f.path = user, path
	return f.data, "image/png", nil
}

func (f *fakeGenerator) Generate(_ context.Context, _ *config.Logger, w *comfy_ui.Workflow, _, negative string, strength *float64, input []byte, aspect string) (comfy_ui.Generation, error) {
	f.called++
	f.workflow, f.input, f.negative, f.strength, f.aspect = w, append([]byte(nil), input...), negative, strength, aspect
	return f.result, nil
}

func imageContext(images ...requestctx.InputImage) context.Context {
	ctx := requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "user-1", Gateway: "discord", ExternalID: "external-1", Assurance: identity.AssuranceDiscordGateway})
	return requestctx.WithInputImages(ctx, images)
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHandlerOutputAndSelection(t *testing.T) {
	data := pngBytes(t)
	text, edit := &comfy_ui.Workflow{}, &comfy_ui.Workflow{}
	for _, tc := range []struct {
		name   string
		args   map[string]interface{}
		mode   string
		source bool
		aspect string
	}{
		{"text default", map[string]interface{}{"prompt": "a lighthouse"}, "text_to_image", false, "landscape"},
		{"edit portrait", map[string]interface{}{"prompt": "moonlit lighthouse", "image_url": "/managed/source.png", "aspect_ratio": "portrait"}, "image_to_image", true, "portrait"},
		{"edit HTTPS", map[string]interface{}{"prompt": "moonlit lighthouse", "image_url": "https://images.example.org/source.png"}, "image_to_image", true, "landscape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeGenerator{result: comfy_ui.Generation{Image: comfy_ui.GeneratedImage{Data: data, MIMEType: "image/png", Size: image.Pt(2, 3)}, Seed: 42, CleanupFailed: true}}
			cache := &fakeSourceCache{data: data}
			handler := newHandler(text, edit, fake, cache, config.NewLogger(config.LevelError))
			result, err := handler(imageContext(), tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if fake.called != 1 || fake.aspect != tc.aspect || fake.negative != "" || fake.strength != nil || (len(fake.input) > 0) != tc.source {
				t.Fatalf("provider args: %+v", fake)
			}
			if (fake.workflow == edit) != tc.source {
				t.Fatal("incorrect workflow")
			}
			if tc.source && (cache.user != "user-1" || (cache.path != "/managed/source.png" && cache.url != "https://images.example.org/source.png")) {
				t.Fatalf("source selection: %+v", cache)
			}
			if !result.IsDegraded || result.ReasonCode != "vram_cleanup_failed" || len(result.Attachments) != 1 || !reflect.DeepEqual(result.Attachments[0].Data, data) {
				t.Fatalf("result: %+v", result)
			}
			var metadata map[string]interface{}
			if err := json.Unmarshal([]byte(result.Content), &metadata); err != nil {
				t.Fatal(err)
			}
			if len(metadata) != 7 || metadata["status"] != "generated" || metadata["mode"] != tc.mode || metadata["mime_type"] != "image/png" || metadata["width"] != float64(2) || metadata["height"] != float64(3) || metadata["seed"] != float64(42) || metadata["attachment_count"] != float64(1) || metadata["image"] != nil {
				t.Fatalf("metadata: %s", result.Content)
			}
		})
	}
}

func TestHandlerRejectsUntrustedSourcesAndArguments(t *testing.T) {
	for name, args := range map[string]map[string]interface{}{
		"HTTP URL":          {"prompt": "edit", "image_url": "http://example.com/image.png"},
		"relative path":     {"prompt": "edit", "image_url": "../current-1"},
		"missing source":    {"prompt": "edit", "image_url": "unknown"},
		"empty source":      {"prompt": "edit", "image_url": ""},
		"non-string source": {"prompt": "edit", "image_url": 1},
		"bad aspect":        {"prompt": "new", "aspect_ratio": "wide"},
		"null aspect":       {"prompt": "new", "aspect_ratio": nil},
		"negative prompt":   {"prompt": "new", "negative_prompt": "bad"},
		"strength":          {"prompt": "new", "strength": 0.5},
		"variant":           {"prompt": "new", "create_variant": true},
		"empty prompt":      {"prompt": " "},
		"oversized prompt":  {"prompt": strings.Repeat("a", 2001)},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeGenerator{}
			handler := newHandler(nil, nil, fake, &fakeSourceCache{data: pngBytes(t)}, config.NewLogger(config.LevelError))
			_, err := handler(imageContext(), args)
			if err == nil || fake.called != 0 {
				t.Fatalf("err=%v provider calls=%d", err, fake.called)
			}
		})
	}
}

func TestHandlerManagedPathOwnership(t *testing.T) {
	cache := imagecache.New(t.TempDir())
	path, err := cache.Save(context.Background(), "user-1", pngBytes(t), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	log := config.NewLogger(config.LevelError)
	for _, tc := range []struct {
		name, path string
		valid      bool
	}{
		{"owned", path, true},
		{"other user", path, false},
		{"arbitrary path", filepath.Join(t.TempDir(), "source.png"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := imageContext()
			if tc.name == "other user" {
				ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "user-2", Gateway: "discord", ExternalID: "external-2", Assurance: identity.AssuranceDiscordGateway})
			}
			fake := &fakeGenerator{result: comfy_ui.Generation{Image: comfy_ui.GeneratedImage{Data: pngBytes(t), MIMEType: "image/png", Size: image.Pt(2, 3)}}}
			_, err := newHandler(nil, nil, fake, cache, log)(ctx, map[string]interface{}{"prompt": "edit", "image_url": tc.path})
			if (err == nil) != tc.valid || (fake.called == 1) != tc.valid {
				t.Fatalf("err=%v calls=%d", err, fake.called)
			}
		})
	}
}

func TestHandlerRejectsUnsafeHTTPSBeforeGeneration(t *testing.T) {
	fake := &fakeGenerator{}
	handler := newHandler(nil, nil, fake, imagecache.New(t.TempDir()), config.NewLogger(config.LevelError))
	for _, source := range []string{"https://127.0.0.1/image.png", "https://user:password@example.com/image.png", "https://example.com/image.png#fragment"} {
		if _, err := handler(imageContext(), map[string]interface{}{"prompt": "edit", "image_url": source}); err == nil || fake.called != 0 {
			t.Fatalf("source=%q err=%v provider calls=%d", source, err, fake.called)
		}
	}
}
