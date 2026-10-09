// Package vision_analyze loads bounded image inputs for the active conversation.
package vision_analyze

import "github.com/jonahgcarpenter/oswald-ai/internal/llm"

// Name is the model-facing vision tool name.
const Name = "vision_analyze"

// Definition returns the model-facing vision loading contract.
func Definition() llm.ToolDefinition {
	four := 4
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Load an image into the conversation so you can see it. Call it when the user references a previous image that is not currently visible, then answer from what you see. Current attached images are already visible. Loads a still image (the first frame of a GIF), not a separate model's analysis.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"image_url": {Type: "string", Description: "Public HTTP/HTTPS image URL, an absolute path to an unexpired image in the current profile's managed image cache, or a base64 data URL for PNG/JPEG/GIF/WebP. Other local files are not allowed."},
				"question":  {Type: "string", Description: "Your question or request about the image (1–2000 characters)."},
				"region":    {Type: "array", Items: &llm.ToolParameterProperty{Type: "integer"}, MinItems: &four, MaxItems: &four, Description: "Optional [x1, y1, x2, y2] crop in source-image pixel coordinates; x2/y2 are exclusive. Applied before further downscaling. Load the full image first to get source dimensions, then re-call to zoom into detail. Cached attachments may already have been resized; original detail cannot be recovered."},
			},
			Required: []string{"image_url", "question"},
		},
	}
}
