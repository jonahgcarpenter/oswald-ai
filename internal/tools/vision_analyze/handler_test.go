package vision_analyze

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func visionContext(profile, gateway string) context.Context {
	return requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: profile, ExternalID: "synthetic", Gateway: gateway, Assurance: identity.AssuranceDiscordGateway})
}

func sourcePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 20), G: uint8(y * 20), A: 128})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHandlerSourcesAndCrop(t *testing.T) {
	data := sourcePNG(t)
	cache := imagecache.New(t.TempDir())
	path, err := cache.Save(context.Background(), "alice", data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	for _, source := range []string{path, dataURL, "http://public.example/image", "https://public.example/image"} {
		t.Run(strings.Split(source, ":")[0], func(t *testing.T) {
			downloads := 0
			handler := newHandler(cache, func(ctx context.Context, raw string) ([]byte, string, error) {
				downloads++
				return data, "image/png", nil
			})
			result, err := handler(visionContext("alice", "discord"), map[string]interface{}{"image_url": source, "question": "Read this", "region": []interface{}{float64(2), float64(1), float64(5), float64(4)}})
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(source, "http") != (downloads == 1) || result.Outcome != governance.OutcomeProductive || len(result.Images) != 1 || len(result.Attachments) != 0 {
				t.Fatalf("downloads=%d images=%d attachments=%d", downloads, len(result.Images), len(result.Attachments))
			}
			metadata := result.Images[0].Geometry
			if metadata == nil || metadata.SourceWidth != 8 || metadata.SourceHeight != 6 || metadata.Width != 3 || metadata.Height != 3 || metadata.Region == nil || *metadata.Region != ([4]int{2, 1, 5, 4}) {
				t.Fatalf("metadata=%+v", metadata)
			}
			if !strings.Contains(result.Content, "Question: Read this") || !strings.Contains(result.Content, "Source image: 8×6.") || !strings.Contains(result.Content, "source region [2, 1, 5, 4]") || !strings.Contains(result.Content, "x = displayed x × 1.00 + 2") {
				t.Fatalf("missing readable tool-result context: %q", result.Content)
			}
			encoded, err := base64.StdEncoding.DecodeString(result.Images[0].Data)
			if err != nil {
				t.Fatal(err)
			}
			decoded, _, err := image.Decode(bytes.NewReader(encoded))
			if err != nil || decoded.Bounds().Dx() != 3 || decoded.Bounds().Dy() != 3 {
				t.Fatalf("crop decode: %v", err)
			}
			pixel := color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA)
			if pixel != (color.NRGBA{R: 40, G: 20, A: 128}) {
				t.Fatalf("crop used wrong source pixel: %+v", pixel)
			}
		})
	}
}

func TestHandlerRejectsInvalidArgumentsAndSources(t *testing.T) {
	data := sourcePNG(t)
	valid := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	handler := newHandler(nil, func(context.Context, string) ([]byte, string, error) {
		return nil, "", errors.New("private source canary")
	})
	cases := []map[string]interface{}{
		{}, {"question": ""}, {"question": strings.Repeat("q", 2001)}, {"question": 1},
		{"unknown": true}, {"image_url": " "}, {"image_url": "relative.png"}, {"image_url": "/etc/passwd"},
		{"image_url": "file:///etc/passwd"}, {"image_url": "https://public.example/image"},
		{"image_url": "data:image/png,raw"}, {"image_url": "data:text/plain;base64,YQ=="},
		{"image_url": "data:image/png;base64,broken"}, {"image_url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)},
		{"image_url": "data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(media.MaxOutputAttachmentBytes)+1)},
		{"region": nil}, {"region": []interface{}{0, 0, 1}}, {"region": []interface{}{0, 0, 1, 2, 3}},
		{"region": []interface{}{0, 0, 0, 1}}, {"region": []interface{}{2, 0, 1, 1}},
		{"region": []interface{}{-1, 0, 1, 1}}, {"region": []interface{}{0, 0, 0.5, 1}},
		{"region": []interface{}{0, 0, math.NaN(), 1}}, {"region": []interface{}{0, 0, math.Inf(1), 1}},
		{"region": []interface{}{0, 0, "1", 1}}, {"region": []interface{}{0, 0, 9, 6}},
	}
	for i, overrides := range cases {
		args := map[string]interface{}{"image_url": valid, "question": "Inspect"}
		if i == 0 {
			args = map[string]interface{}{}
		}
		for key, value := range overrides {
			args[key] = value
		}
		result, err := handler(visionContext("alice", "discord"), args)
		if err == nil || len(result.Images) != 0 || strings.Contains(err.Error(), "canary") {
			t.Fatalf("case %d: images=%d err=%v", i, len(result.Images), err)
		}
	}
}

func TestHandlerOwnershipExpiryAndCancellation(t *testing.T) {
	cache := imagecache.New(t.TempDir())
	path, err := cache.Save(context.Background(), "alice", sourcePNG(t), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(cache)
	args := map[string]interface{}{"image_url": path, "question": "Inspect"}
	alias := filepath.Join(filepath.Dir(path), strings.Repeat("a", 32)+".png")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := handler(visionContext("alice", "discord"), map[string]interface{}{"image_url": alias, "question": "Inspect"}); err == nil {
		t.Fatal("cache symlink load succeeded")
	}
	for _, ctx := range []context.Context{context.Background(), visionContext("bob", "discord"), visionContext("alice", "openai")} {
		if _, err := handler(ctx, args); err == nil {
			t.Fatal("unauthorized image load succeeded")
		}
	}
	ctx, cancel := context.WithCancel(visionContext("alice", "discord"))
	cancel()
	if _, err := handler(ctx, args); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	expired := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, expired, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := handler(visionContext("alice", "discord"), args); err == nil {
		t.Fatal("expired image load succeeded")
	}
	ctx, cancel = context.WithCancel(visionContext("alice", "discord"))
	defer cancel()
	handler = newHandler(cache, func(context.Context, string) ([]byte, string, error) {
		cancel()
		return nil, "", errors.New("network failure")
	})
	if _, err := handler(ctx, map[string]interface{}{"image_url": "https://public.example/image", "question": "Inspect"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation=%v", err)
	}
}

func TestDefinition(t *testing.T) {
	def := Definition()
	region := def.Parameters.Properties["region"]
	if def.Name != Name || len(def.Parameters.Properties) != 3 || len(def.Parameters.Required) != 2 || def.Parameters.Required[0] != "image_url" || def.Parameters.Required[1] != "question" || region.Type != "array" || region.Items == nil || region.Items.Type != "integer" || region.MinItems == nil || *region.MinItems != 4 || region.MaxItems == nil || *region.MaxItems != 4 {
		t.Fatalf("invalid vision tool definition: %+v", def)
	}
}
