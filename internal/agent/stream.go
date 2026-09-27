package agent

import (
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
)

// StreamChunkType identifies the kind of content in a StreamChunk.
type StreamChunkType string

const (
	// ChunkThinking carries tokens from the model's internal reasoning phase.
	ChunkThinking StreamChunkType = "thinking"

	// ChunkContent carries tokens from the model's visible response.
	ChunkContent StreamChunkType = "content"

	// ChunkStatus carries status messages injected by the agent (e.g. "[Calling: web_search]").
	ChunkStatus StreamChunkType = "status"

	// ChunkToolCall carries structured tool invocation data for frontend timelines.
	ChunkToolCall StreamChunkType = "tool_call"

	// ChunkToolResult carries structured tool result data for frontend timelines.
	ChunkToolResult StreamChunkType = "tool_result"
)

// ToolStreamSearchResult is a UI-safe search result emitted for web_search tools.
type ToolStreamSearchResult struct {
	Title       string   `json:"title,omitempty"`
	URL         string   `json:"url,omitempty"`
	Domain      string   `json:"domain,omitempty"`
	Content     string   `json:"content,omitempty"`
	Engines     []string `json:"engines,omitempty"`
	PublishedAt string   `json:"published_at,omitempty"`
	Score       float64  `json:"score,omitempty"`
}

// ToolStreamSearchPayload contains structured web_search details for streaming UIs.
type ToolStreamSearchPayload struct {
	Query               string                   `json:"query,omitempty"`
	Results             []ToolStreamSearchResult `json:"results,omitempty"`
	IsDegraded          bool                     `json:"is_degraded,omitempty"`
	UnresponsiveEngines []string                 `json:"unresponsive_engines,omitempty"`
}

// ToolStreamPayload contains structured tool data for frontend rendering.
type ToolStreamPayload struct {
	Name       string                   `json:"name"`
	Arguments  map[string]interface{}   `json:"arguments,omitempty"`
	ResultText string                   `json:"result_text,omitempty"`
	DurationMS int64                    `json:"duration_ms,omitempty"`
	IsError    bool                     `json:"is_error,omitempty"`
	WebSearch  *ToolStreamSearchPayload `json:"web.search,omitempty"`
}

// StreamChunk is a single typed token event streamed to gateways during Process().
// Gateways receive thinking tokens, content tokens, and agent status messages via this type.
type StreamChunk struct {
	Type        StreamChunkType          `json:"type"`
	Text        string                   `json:"text,omitempty"`
	Tool        *ToolStreamPayload       `json:"tool,omitempty"`
	Attachments []media.OutputAttachment `json:"-"`
}

func toolStreamPayload(toolName string, args map[string]interface{}, result string, duration time.Duration, isError bool) *ToolStreamPayload {
	payload := &ToolStreamPayload{
		Name:       toolName,
		Arguments:  args,
		ResultText: result,
		DurationMS: duration.Milliseconds(),
		IsError:    isError,
	}
	if toolName == toolnames.ComfyUITextToImage || toolName == toolnames.ComfyUIImageToImage {
		payload.Arguments = nil
		payload.ResultText = ""
		return payload
	}
	if toolName != toolnames.WebSearch {
		return payload
	}

	searchPayload := &ToolStreamSearchPayload{}
	if query, ok := args["query"].(string); ok {
		searchPayload.Query = strings.TrimSpace(query)
	}
	if !isError {
		response, err := websearch.DecodeToolResponse(result)
		if err == nil {
			searchPayload.IsDegraded = response.Degraded
			searchPayload.UnresponsiveEngines = response.UnresponsiveEngines
			searchPayload.Results = make([]ToolStreamSearchResult, 0, len(response.Results))
			for _, r := range response.Results {
				searchPayload.Results = append(searchPayload.Results, ToolStreamSearchResult{
					Title:       r.Title,
					URL:         r.URL,
					Domain:      r.Domain,
					Content:     r.Snippet,
					Engines:     r.Engines,
					PublishedAt: r.PublishedAt,
					Score:       r.Score,
				})
			}
		}
	}
	payload.WebSearch = searchPayload
	return payload
}
