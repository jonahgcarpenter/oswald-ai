package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

func promptWithAttachedImages(prompt string, images []requestctx.InputImage) string {
	for _, image := range images {
		if image.Path != "" {
			prompt += "\n\n[Image attached at: " + image.Path + "]"
		}
	}
	return prompt
}

// steerImagesOverflow reports whether draining pending steers would push the
// turn past the per-request image bound. Overflow stays pending and auto-runs
// as the next turn instead of failing or silently dropping media.
func steerImagesOverflow(pending []Steer, hostImageCount int) bool {
	total := hostImageCount
	for _, st := range pending {
		total += len(st.Images)
	}
	return total > media.MaxImagesPerRequest
}

// cacheSteerImages persists steered follow-up images to the managed image
// cache so they gain stable paths for annotations and history, mirroring host
// image intake. Unavailable cache or save failures degrade to pathless bytes:
// the model call still carries the images, only the path annotation is lost.
// Never fails the turn.
func (a *Agent) cacheSteerImages(ctx context.Context, senderID string, reqLog *config.Logger, images []llm.InputImage) []requestctx.InputImage {
	ctxImages := make([]requestctx.InputImage, 0, len(images))
	for i, image := range images {
		ctxImages = append(ctxImages, requestctx.InputImage{ID: fmt.Sprintf("steered-%d", i+1), MIMEType: image.MimeType, Data: image.Data, Source: image.Source})
	}
	if len(ctxImages) == 0 || a.imageCache == nil {
		return ctxImages
	}
	for i := range ctxImages {
		data, err := base64.StdEncoding.DecodeString(ctxImages[i].Data)
		if err != nil {
			reqLog.Debug("agent.steer.image_decode_failed", "failed to decode steered image for caching", config.F("status", "degraded"), config.ErrorField(err))
			continue
		}
		assetID := ctxImages[i].ID
		if ctxImages[i].Source != "generated" {
			assetID = config.NewRequestID()
		}
		path, err := a.imageCache.SaveAsset(ctx, senderID, assetID, data, ctxImages[i].MIMEType)
		if err != nil {
			reqLog.Debug("agent.steer.image_cache_failed", "failed to cache steered image", config.F("status", "degraded"), config.ErrorField(err))
			continue
		}
		ctxImages[i].Path = path
	}
	return ctxImages
}

// planGeneratedImage binds catalog paths to logical image versions. Other valid
// paths and HTTPS URLs are left to the handler to validate as new sources.
func planGeneratedImage(args map[string]interface{}, sources, selected []requestctx.InputImage) (requestctx.InputImage, int, error) {
	image := requestctx.InputImage{}
	if raw, exists := args["image_url"]; exists {
		selector, ok := raw.(string)
		if !ok || selector == "" || selector != strings.TrimSpace(selector) {
			return image, -1, fmt.Errorf("image_url must be an image path or HTTPS URL")
		}
		var source requestctx.InputImage
		for _, candidate := range sources {
			if candidate.Path != "" && candidate.Path == selector {
				source = candidate
				break
			}
		}
		if source.ID == "" {
			u, err := url.Parse(selector)
			if !filepath.IsAbs(selector) && (err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil) {
				return image, -1, fmt.Errorf("image_url must be a managed image path or HTTPS URL")
			}
		} else {
			image.ParentSourceImageID = source.ID
			image.ImageID = source.ImageID
		}
	}
	for i, current := range selected {
		if image.ImageID != "" && current.ImageID == image.ImageID {
			return image, i, nil
		}
	}
	if len(selected) >= 4 {
		return image, -1, fmt.Errorf("at most four logical images can be generated per request; edit an existing generated image using its image_url path or ask for another request")
	}
	return image, -1, nil
}
