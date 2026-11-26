// Package video_analyze loads bounded video inputs as still frames for the
// active conversation. Videos ride the exact same image pipeline as
// vision_analyze: sampled frames attach as native vision images, so the model
// sees video the way it sees images and picks this tool instead of the
// former. Motion between frames and the audio track are never perceived.
package video_analyze

import "github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"

// Name is the model-facing video tool name.
const Name = "video_analyze"

// Frame bounds. Four is the default because the turn image budget retains
// the newest four images per batch; more than four frames arrive as labeled
// contact sheets so nothing is silently dropped.
const (
	DefaultFrames = 4
	MaxFrames     = 8
)

// Definition returns the model-facing video loading contract.
func Definition() llm.ToolDefinition {
	four := 4
	eight := 8
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Load a video into the conversation as still frames so you can see it. Use this for video files — for still images use vision_analyze. Either request evenly spaced frames (frames) or exact moments (timestamps); if both are given, timestamps win. Frames arrive as one labeled contact sheet (the single frame attaches directly for a 1-frame request) with per-frame cache paths for full-resolution drill-down. Motion between frames and the audio track are not perceived.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"source":   {Type: "string", Description: "Absolute path to a video in the current profile's managed media cache, an absolute video path inside the current profile workspace, or a public HTTPS video URL. Relative paths, data URLs, and other profiles' files are not allowed."},
				"question": {Type: "string", Description: "Your question or request about the video (1–2000 characters)."},
				"frames":   {Type: "integer", Minimum: float64Ptr(1), Maximum: float64Ptr(8), Description: "Evenly spaced frames to sample, 1–8 (default 4). Ignored when timestamps is set."},
				"timestamps": {
					Type: "array", Items: &llm.ToolParameterProperty{Type: "number"},
					MinItems: &four, MaxItems: &eight,
					Description: "Exact seconds to sample, e.g. [7.0, 8.5] (1–8 entries, each within the video duration). Order preserved. Overrides frames.",
				},
			},
			Required: []string{"source", "question"},
		},
	}
}

func float64Ptr(value float64) *float64 { return &value }
