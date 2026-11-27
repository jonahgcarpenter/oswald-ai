// Package comfy_ui implements the ComfyUI image generation provider.
package comfy_ui

import "image"

const (
	InputFilename       = "oswald-img2img-input.png"
	InputSubfolder      = "oswald"
	InputImageReference = InputSubfolder + "/" + InputFilename
)

// Mode identifies one validated workflow contract.
type Mode string

const (
	TextToImage  Mode = "text_to_image"
	ImageToImage Mode = "image_to_image"
)

// GeneratedImage is a validated image downloaded from ComfyUI.
type GeneratedImage struct {
	Filename string
	MIMEType string
	Data     []byte
	Size     image.Point
}

// Generation contains a validated image and the effective workflow parameters.
// CleanupFailed can be true even if generation failed after the permit was acquired.
type Generation struct {
	Image             GeneratedImage
	Seed              uint32
	EffectiveStrength *float64 // nil for text-to-image
	CleanupFailed     bool
}

type outputDescriptor struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}
