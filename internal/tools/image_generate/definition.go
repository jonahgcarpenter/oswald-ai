package image_generate

import "github.com/jonahgcarpenter/oswald-ai/internal/llm"

// Name is the model-facing image generation tool name.
const Name = "image_generate"

// Definition returns the model-facing image generation contract.
func Definition() llm.ToolDefinition {
	min, max, noExtras := 1, 2000, false
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Generate one image from a concrete visual prompt. The generated image is attached to the final response. Omit image_url to create a new image from text; to edit an image, supply exactly a server-owned source image ID from the available current or generated image catalog as image_url. URLs and local paths are not supported. Aspect ratios are approximate: landscape (default), square, or portrait. The result reports image metadata; the server adds an image ID for subsequent edits. Use source image IDs rather than external image URLs.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"prompt":       {Type: "string", Description: "Describe the desired final image with distinguishing visual details", MinLength: &min, MaxLength: &max},
				"aspect_ratio": {Type: "string", Description: "Approximate output aspect ratio; defaults to landscape", Enum: []string{"landscape", "square", "portrait"}, Default: "landscape"},
				"image_url":    {Type: "string", Description: "Exact server-owned source image ID from the current request image catalog; omit for text-to-image"},
			},
			Required: []string{"prompt"}, AdditionalProperties: &noExtras,
		},
	}
}
