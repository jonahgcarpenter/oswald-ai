package image_generate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
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
		{"edit portrait", map[string]interface{}{"prompt": "moonlit lighthouse", "image_url": "generated-2", "aspect_ratio": "portrait"}, "image_to_image", true, "portrait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeGenerator{result: comfy_ui.Generation{Image: comfy_ui.GeneratedImage{Data: data, MIMEType: "image/png", Size: image.Pt(2, 3)}, Seed: 42, CleanupFailed: true}}
			handler := newHandler(text, edit, fake, config.NewLogger(config.LevelError))
			ctx := imageContext(requestctx.InputImage{ID: "current-1", Data: "bad"}, requestctx.InputImage{ID: "generated-2", Data: base64.StdEncoding.EncodeToString(data)})
			result, err := handler(ctx, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if fake.called != 1 || fake.aspect != tc.aspect || fake.negative != "" || fake.strength != nil || (len(fake.input) > 0) != tc.source {
				t.Fatalf("provider args: %+v", fake)
			}
			if (fake.workflow == edit) != tc.source {
				t.Fatal("incorrect workflow")
			}
			if !result.IsDegraded || result.ReasonCode != "vram_cleanup_failed" || len(result.Attachments) != 1 || !reflect.DeepEqual(result.Attachments[0].Data, data) {
				t.Fatalf("result: %+v", result)
			}
			var metadata map[string]interface{}
			if err := json.Unmarshal([]byte(result.Content), &metadata); err != nil {
				t.Fatal(err)
			}
			if len(metadata) != 7 || metadata["status"] != "generated" || metadata["mode"] != tc.mode || metadata["mime_type"] != "image/png" || metadata["width"] != float64(2) || metadata["height"] != float64(3) || metadata["seed"] != float64(42) || metadata["attachment_count"] != float64(1) || strings.Contains(result.Content, base64.StdEncoding.EncodeToString(data)) {
				t.Fatalf("metadata: %s", result.Content)
			}
		})
	}
}

func TestHandlerRejectsUntrustedSourcesAndArguments(t *testing.T) {
	data := pngBytes(t)
	for name, args := range map[string]map[string]interface{}{
		"external URL":      {"prompt": "edit", "image_url": "https://example.com/image.png"},
		"local path":        {"prompt": "edit", "image_url": "/tmp/current-1"},
		"missing source":    {"prompt": "edit", "image_url": "unknown"},
		"empty source":      {"prompt": "edit", "image_url": ""},
		"non-string source": {"prompt": "edit", "image_url": 1},
		"bad aspect":        {"prompt": "new", "aspect_ratio": "wide"},
		"null aspect":       {"prompt": "new", "aspect_ratio": nil},
		"negative prompt":   {"prompt": "new", "negative_prompt": "bad"},
		"strength":          {"prompt": "new", "strength": 0.5},
		"variant":           {"prompt": "new", "create_variant": true},
		"empty prompt":      {"prompt": " "},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeGenerator{}
			handler := newHandler(nil, nil, fake, config.NewLogger(config.LevelError))
			_, err := handler(imageContext(requestctx.InputImage{ID: "current-1", Data: base64.StdEncoding.EncodeToString(data)}), args)
			if err == nil || fake.called != 0 {
				t.Fatalf("err=%v provider calls=%d", err, fake.called)
			}
		})
	}
	fake := &fakeGenerator{}
	_, err := newHandler(nil, nil, fake, config.NewLogger(config.LevelError))(imageContext(), map[string]interface{}{"prompt": "edit", "image_url": "current-1"})
	if err == nil || fake.called != 0 {
		t.Fatal("accepted source without catalog")
	}
}
