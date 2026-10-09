package agent

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func promptWithAttachedImages(prompt string, images []requestctx.InputImage) string {
	for _, image := range images {
		if image.Path != "" {
			prompt += "\n\n[Image attached at: " + image.Path + "]"
		}
	}
	return prompt
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
		return image, -1, fmt.Errorf("at most four logical images can be delivered per request; edit an existing generated image using its image_url path or ask for another request")
	}
	return image, -1, nil
}
