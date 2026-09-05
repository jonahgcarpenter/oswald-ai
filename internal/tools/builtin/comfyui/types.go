// Package comfyui implements first-party ComfyUI image generation tools.
package comfyui

import "image"

const (
	InputFilename       = "oswald-img2img-input.png"
	InputSubfolder      = "oswald"
	InputImageReference = InputSubfolder + "/" + InputFilename
	maxPromptRunes      = 2000
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

type outputDescriptor struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

type toolMetadata struct {
	Status          string `json:"status"`
	Mode            Mode   `json:"mode"`
	MIMEType        string `json:"mime_type"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	Seed            uint32 `json:"seed"`
	AttachmentCount int    `json:"attachment_count"`
}
