package image_generate

import "github.com/jonahgcarpenter/oswald-ai/internal/llm"

// Name is the model-facing image generation tool name.
const Name = "image_generate"

// Definition returns the model-facing image generation contract.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Generate an image from a text description, or transform an existing image by supplying image_url. For transformations, describe the complete desired final image and the details to retain. Editing affects the whole image and may change faces, geometry, or other details; exact preservation and localized edits are not guaranteed. Returns the generated image as a private absolute file path in the image field, with the image delivered as an attachment.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"prompt":       {Type: "string", Description: "Describe the desired final image, including subject, composition, style, lighting, and colors. When editing, clearly describe the change and the existing details to retain."},
				"aspect_ratio": {Type: "string", Description: "Output aspect ratio: landscape is 1280x720, square is 1024x1024, and portrait is 720x1280. When editing, match the source ratio where possible to avoid center cropping.", Enum: []string{"landscape", "square", "portrait"}, Default: "landscape"},
				"image_url":    {Type: "string", Description: "Optional source image: a public HTTPS image URL or an absolute path to an unexpired image in the current user's managed image cache. Omit to generate a new image from text."},
			},
			Required: []string{"prompt"},
		},
	}
}
