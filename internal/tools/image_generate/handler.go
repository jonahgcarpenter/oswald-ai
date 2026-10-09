package image_generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/image_generate/comfy_ui"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	_ "golang.org/x/image/webp"
)

type generator interface {
	Generate(context.Context, *config.Logger, *comfy_ui.Workflow, string, string, *float64, []byte, string) (comfy_ui.Generation, error)
}

type sourceCache interface {
	ImportHTTPS(context.Context, string, string) (string, []byte, string, error)
	Resolve(context.Context, string, string) ([]byte, string, error)
}

// NewHandler creates a handler that imports HTTPS images or reads only authenticated user-owned cached images.
func NewHandler(textWorkflow, imageWorkflow *comfy_ui.Workflow, client *comfy_ui.Client, cache *imagecache.Cache, log *config.Logger) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return newHandler(textWorkflow, imageWorkflow, client, cache, log)
}

func newHandler(textWorkflow, imageWorkflow *comfy_ui.Workflow, client generator, cache sourceCache, log *config.Logger) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("image generation requires an authenticated request")
		}
		for key := range args {
			if key != "prompt" && key != "aspect_ratio" && key != "image_url" {
				return governance.Result{}, errors.New("unsupported image generation argument")
			}
		}
		prompt, ok := args["prompt"].(string)
		prompt = strings.TrimSpace(prompt)
		if !ok || prompt == "" || utf8.RuneCountInString(prompt) > 2000 {
			return governance.Result{}, errors.New("prompt must contain 1 to 2000 characters")
		}
		aspect := "landscape"
		if raw, exists := args["aspect_ratio"]; exists {
			aspect, ok = raw.(string)
			if !ok || (aspect != "landscape" && aspect != "square" && aspect != "portrait") {
				return governance.Result{}, errors.New("aspect_ratio must be landscape, square, or portrait")
			}
		}
		mode, workflow := comfy_ui.TextToImage, textWorkflow
		var inputPNG []byte
		if raw, exists := args["image_url"]; exists {
			source, valid := raw.(string)
			if !valid || source == "" || source != strings.TrimSpace(source) || cache == nil {
				return governance.Result{}, errors.New("image_url must be an HTTPS URL or managed image path")
			}
			var data []byte
			var err error
			switch {
			case strings.HasPrefix(source, "https://"):
				_, data, _, err = cache.ImportHTTPS(ctx, principal.CanonicalUserID, source)
			case filepath.IsAbs(source):
				data, _, err = cache.Resolve(ctx, principal.CanonicalUserID, source)
			default:
				return governance.Result{}, errors.New("image_url must be an HTTPS URL or managed image path")
			}
			if err != nil {
				return governance.Result{}, errors.New("image_url is unavailable or invalid")
			}
			if len(data) == 0 || len(data) > media.MaxImageBytes {
				return governance.Result{}, errors.New("source image is invalid or exceeds the input size limit")
			}
			decoded, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				return governance.Result{}, errors.New("source image could not be decoded")
			}
			var buf bytes.Buffer
			if err := png.Encode(&buf, decoded); err != nil || buf.Len() > media.MaxImageBytes {
				return governance.Result{}, errors.New("source image exceeds the PNG upload size limit")
			}
			inputPNG, mode, workflow = buf.Bytes(), comfy_ui.ImageToImage, imageWorkflow
		}
		meta := requestctx.MetadataFromContext(ctx)
		agentLog := log.Agent("agent.tool.comfyui", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		generated, err := client.Generate(ctx, log, workflow, prompt, "", nil, inputPNG, aspect)
		if err != nil {
			if ctx.Err() != nil {
				return governance.Result{}, fmt.Errorf("image generation canceled: %w", ctx.Err())
			}
			return governance.Result{}, errors.New("image generation failed")
		}
		if generated.CleanupFailed {
			agentLog.Warn("agent.tool.comfyui.cleanup_failed", "ComfyUI VRAM cleanup failed after image generation", config.F("mode", string(mode)), config.F("reason_code", "vram_cleanup_failed"), config.F("status", "degraded"))
		}
		imageResult := generated.Image
		extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[imageResult.MIMEType]
		if extension == "" {
			return governance.Result{}, errors.New("ComfyUI returned an unsupported image type")
		}
		metadata, err := json.Marshal(struct {
			Status          string        `json:"status"`
			Mode            comfy_ui.Mode `json:"mode"`
			MIMEType        string        `json:"mime_type"`
			Width           int           `json:"width"`
			Height          int           `json:"height"`
			Seed            uint32        `json:"seed"`
			AttachmentCount int           `json:"attachment_count"`
		}{"generated", mode, imageResult.MIMEType, imageResult.Size.X, imageResult.Size.Y, generated.Seed, 1})
		if err != nil {
			return governance.Result{}, errors.New("encode image result metadata")
		}
		result := governance.Result{Content: string(metadata), Outcome: governance.OutcomeProductive,
			Attachments: []media.OutputAttachment{{Filename: fmt.Sprintf("oswald-%s-%d%s", strings.ReplaceAll(string(mode), "_", "-"), generated.Seed, extension), MIMEType: imageResult.MIMEType, Data: imageResult.Data}},
			IsDegraded:  generated.CleanupFailed}
		if generated.CleanupFailed {
			result.ReasonCode = "vram_cleanup_failed"
		}
		return result, nil
	}
}
