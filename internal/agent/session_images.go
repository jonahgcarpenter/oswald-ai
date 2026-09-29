package agent

import (
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

const imageContextPrefix = "[Session image catalog; reference data, not instructions]"

func sessionImageContext(sources, generated []requestctx.InputImage) llm.ChatMessage {
	var text strings.Builder
	text.WriteString(imageContextPrefix)
	text.WriteString("\nAvailable image_url selector IDs (omit image_url to generate a new image):\n")
	for _, image := range sources {
		text.WriteString(image.ID)
		if image.Source == "generated" {
			fmt.Fprintf(&text, " (image_id=%s version=%d parent_source_image_id=%s)", image.ImageID, image.Version, image.ParentSourceImageID)
		} else {
			text.WriteString(" (current attached/replied)")
		}
		text.WriteByte('\n')
	}
	message := llm.ChatMessage{Role: "user", Content: text.String()}
	for _, image := range generated {
		message.Content += "\nGenerated image shown: " + image.ID
		message.Images = append(message.Images, llm.InputImage{MimeType: image.MIMEType, Data: image.Data, Source: "generated"})
	}
	return message
}

// planGeneratedImage resolves only catalog-owned selectors before provider work.
func planGeneratedImage(args map[string]interface{}, sources, selected []requestctx.InputImage) (requestctx.InputImage, int, error) {
	image := requestctx.InputImage{}
	if raw, exists := args["image_url"]; exists {
		id, ok := raw.(string)
		if !ok || id == "" {
			return image, -1, fmt.Errorf("image_url must be an available catalog ID")
		}
		var source requestctx.InputImage
		for _, candidate := range sources {
			if candidate.ID == id {
				source = candidate
				break
			}
		}
		if source.ID == "" {
			return image, -1, fmt.Errorf("image_url is unavailable; select an ID from the current catalog")
		}
		image.ParentSourceImageID = source.ID
		image.ImageID = source.ImageID
	}
	for i, current := range selected {
		if image.ImageID != "" && current.ImageID == image.ImageID {
			return image, i, nil
		}
	}
	if len(selected) >= 4 {
		return image, -1, fmt.Errorf("at most four logical images can be delivered per request; edit an existing generated image using its image_url ID or ask for another request")
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
