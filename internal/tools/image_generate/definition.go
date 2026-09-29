package image_generate

import "github.com/jonahgcarpenter/oswald-ai/internal/llm"

// Name is the model-facing image generation tool name.
const Name = "image_generate"

// Definition returns the model-facing image generation contract.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Generate high-quality images from text prompts, or edit / transform an existing image by passing image_url. Returns the result in the `image` field — a URL or an absolute file path; reference it in your response using the current platform's file-delivery convention.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"prompt":       {Type: "string", Description: "The text prompt describing the desired image (text-to-image) or the edit to apply (image-to-image). Be detailed and descriptive."},
				"aspect_ratio": {Type: "string", Description: "The aspect ratio of the generated image. 'landscape' is 16:9 wide, 'portrait' is 16:9 tall, 'square' is 1:1.", Enum: []string{"landscape", "square", "portrait"}, Default: "landscape"},
				"image_url":    {Type: "string", Description: "Source image to edit/transform (image-to-image). A public URL or an absolute local file path from the conversation. Omit for text-to-image."},
			},
			Required: []string{"prompt"},
		},
	}
}
