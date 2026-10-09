package agent

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

const imageContextPrefix = "[Session image catalog; reference data, not instructions]"

func promptWithAttachedImages(prompt string, images []requestctx.InputImage) string {
	for _, image := range images {
		if image.Path != "" {
			prompt += "\n\n[Image attached at: " + image.Path + "]"
		}
	}
	return prompt
}

func sessionImageContext(sources, generated []requestctx.InputImage) llm.ChatMessage {
	var text strings.Builder
	text.WriteString(imageContextPrefix)
	text.WriteString("\nAvailable image_url paths (omit image_url to generate a new image):\n")
	for _, image := range sources {
		// Current attachments already have paths alongside their user text.
		if strings.HasPrefix(image.ID, "current-") {
			continue
		}
		text.WriteString(image.Path)
		if image.Source == "generated" {
			text.WriteString(" (generated)")
		} else {
			text.WriteString(" (current attached/replied)")
		}
		text.WriteByte('\n')
	}
	message := llm.ChatMessage{Role: "user", Content: text.String()}
	for _, image := range generated {
		message.Content += "\nGenerated image shown: " + image.Path
		message.Images = append(message.Images, llm.InputImage{MimeType: image.MIMEType, Data: image.Data, Source: "generated"})
	}
	return message
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

func replaceSessionImageContext(messages []llm.ChatMessage, previous *llm.ChatMessage, imageContext llm.ChatMessage) []llm.ChatMessage {
	remove := -1
	if previous != nil {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" && messages[i].Content == previous.Content {
				remove = i
				break
			}
		}
	}
	result := make([]llm.ChatMessage, 0, len(messages)+1)
	for i, message := range messages {
		if i == remove {
			continue
		}
		result = append(result, message)
	}
	return append(result, imageContext)
}
