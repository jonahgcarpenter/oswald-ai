// Package browser exposes local headless browser automation to the model.
// All behavior assumes an on-machine Chromium; there are no hosted or cloud
// dependencies. Sessions are profile-scoped: one profile never drives
// another profile's browser.
package browser

import "github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"

// Model-facing browser tool names.
const (
	NavigateName  = "browser_navigate"
	SnapshotName  = "browser_snapshot"
	ClickName     = "browser_click"
	TypeName      = "browser_type"
	ScrollName    = "browser_scroll"
	BackName      = "browser_back"
	PressName     = "browser_press"
	GetImagesName = "browser_get_images"
	VisionName    = "browser_vision"
	ConsoleName   = "browser_console"
)

// Names returns the browser tool names in registration order.
func Names() []string {
	return []string{NavigateName, SnapshotName, ClickName, TypeName, ScrollName, BackName, PressName, GetImagesName, VisionName, ConsoleName}
}

// Definitions returns the model-facing browser contracts in registration order.
func Definitions() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		NavigateDefinition(), SnapshotDefinition(), ClickDefinition(), TypeDefinition(),
		ScrollDefinition(), BackDefinition(), PressDefinition(), GetImagesDefinition(),
		VisionDefinition(), ConsoleDefinition(),
	}
}

// NavigateDefinition opens a URL and returns a compact snapshot.
func NavigateDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NavigateName,
		Description: "Navigate to a URL in the browser. Initializes the session and loads the page. Must be called before other browser tools. For simple information retrieval, prefer a lightweight retrieval tool when one is available (faster, cheaper). For plain-text endpoints — URLs ending in .md, .txt, .json, .yaml, .yml, .csv, .xml, raw.githubusercontent.com, or any documented API endpoint — prefer an available text-extraction or terminal-fetch tool; the browser stack is overkill and much slower for these. Use browser tools when you need to interact with a page (click, fill forms, dynamic content). Returns a compact page snapshot with interactive elements and ref IDs — no need to call browser_snapshot separately after navigating.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"url": {Type: "string", Description: "The URL to navigate to (e.g., 'https://example.com')"},
			},
			Required:             []string{"url"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// SnapshotDefinition reads the current accessibility tree.
func SnapshotDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        SnapshotName,
		Description: "Get a text-based snapshot of the current page's accessibility tree. Returns interactive elements with ref IDs (like @e1, @e2) for browser_click and browser_type. full=false (default): compact view with interactive elements. full=true: complete page content. Snapshots over 15000 chars are truncated or summarized; when that happens the complete snapshot is saved to a file and the output includes its path so you can page through the rest. Requires browser_navigate first. Note: browser_navigate already returns a compact snapshot — use this to refresh after interactions that change the page, or with full=true for complete content.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"full": {Type: "boolean", Description: "If true, returns complete page content. If false (default), returns compact view with interactive elements only.", Default: false},
			},
			Required:             []string{},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// ClickDefinition activates an element by ref.
func ClickDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        ClickName,
		Description: "Click on an element identified by its ref ID from the snapshot (e.g., '@e5'). The ref IDs are shown in square brackets in the snapshot output. Requires browser_navigate and browser_snapshot to be called first.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"ref": {Type: "string", Description: "The element reference from the snapshot (e.g., '@e5', '@e12')"},
			},
			Required:             []string{"ref"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// TypeDefinition types into an input by ref.
func TypeDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        TypeName,
		Description: "Type text into an input field identified by its ref ID. Clears the field first, then types the new text. Requires browser_navigate and browser_snapshot to be called first.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"ref":  {Type: "string", Description: "The element reference from the snapshot (e.g., '@e3')"},
				"text": {Type: "string", Description: "The text to type into the field"},
			},
			Required:             []string{"ref", "text"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// ScrollDefinition moves the viewport.
func ScrollDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        ScrollName,
		Description: "Scroll the page in a direction. Use this to reveal more content that may be below or above the current viewport. Requires browser_navigate to be called first.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"direction": {Type: "string", Description: "Direction to scroll", Enum: []string{"up", "down"}},
			},
			Required:             []string{"direction"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// BackDefinition moves history back.
func BackDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        BackName,
		Description: "Navigate back to the previous page in browser history. Requires browser_navigate to be called first.",
		Parameters: llm.ToolParameters{
			Type:                 "object",
			Properties:           map[string]llm.ToolParameterProperty{},
			Required:             []string{},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// PressDefinition dispatches a keyboard key.
func PressDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        PressName,
		Description: "Press a keyboard key. Useful for submitting forms (Enter), navigating (Tab), or keyboard shortcuts. Requires browser_navigate to be called first.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"key": {Type: "string", Description: "Key to press (e.g., 'Enter', 'Tab', 'Escape', 'ArrowDown')"},
			},
			Required:             []string{"key"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// GetImagesDefinition lists page images.
func GetImagesDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        GetImagesName,
		Description: "Get a list of all images on the current page with their URLs and alt text. Useful for finding images to analyze with the vision tool. Requires browser_navigate to be called first.",
		Parameters: llm.ToolParameters{
			Type:                 "object",
			Properties:           map[string]llm.ToolParameterProperty{},
			Required:             []string{},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// VisionDefinition captures a screenshot for visual inspection.
func VisionDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        VisionName,
		Description: "Take a screenshot of the current page so you can inspect it visually. Use this when you need to understand what the page looks like - especially for CAPTCHAs, visual verification challenges, complex layouts, or cases where the text snapshot misses important visual information. When the active model has native vision, the screenshot is attached to your context directly and you inspect it on the next turn; otherwise the harness falls back to an auxiliary vision model and returns a text analysis. Includes a screenshot_path that you can share with the user. Requires browser_navigate to be called first.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"question": {Type: "string", Description: "What you want to know about the page visually. Be specific about what you're looking for."},
				"annotate": {Type: "boolean", Description: "If true, overlay numbered [N] labels on interactive elements. Each [N] maps to ref @eN for subsequent browser commands. Useful for QA and spatial reasoning about page layout.", Default: false},
			},
			Required:             []string{"question"},
			AdditionalProperties: boolPointer(false),
		},
	}
}

// ConsoleDefinition reads console output or evaluates JS.
func ConsoleDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        ConsoleName,
		Description: "Get browser console output and JavaScript errors from the current page. Returns console.log/warn/error/info messages and uncaught JS exceptions. Use this to detect silent JavaScript errors, failed API calls, and application warnings. Requires browser_navigate to be called first. When 'expression' is provided, evaluates JavaScript in the page context and returns the result — use this for DOM inspection, reading page state, or extracting data programmatically.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"clear":      {Type: "boolean", Description: "If true, clear the message buffers after reading", Default: false},
				"expression": {Type: "string", Description: "JavaScript expression to evaluate in the page context. Runs in the browser like DevTools console — full access to DOM, window, document. Return values are serialized to JSON. Example: 'document.title' or 'document.querySelectorAll(\"a\").length'"},
			},
			Required:             []string{},
			AdditionalProperties: boolPointer(false),
		},
	}
}

func boolPointer(value bool) *bool { return &value }
