// Package openai exposes a stateless OpenAI-compatible chat gateway.
package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const maxBody = 2 << 20

var clientToolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// clientParticipantNameRE bounds the OpenAI participant name field.
var clientParticipantNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// parseParticipantName validates the optional message name field. Tool
// messages never take a name; other roles accept the spec charset.

func parseParticipantName(msg chatMessage) (string, bool) {
	if msg.Name == nil {
		return "", true
	}
	name := strings.TrimSpace(*msg.Name)
	if msg.Role == "tool" || !clientParticipantNameRE.MatchString(name) {
		return "", false
	}
	return name, true
}

// Gateway serves the stateless OpenAI-compatible API.

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             *string         `json:"name"`
	ReasoningContent *string         `json:"reasoning_content"`
	ToolCalls        []wireToolCall  `json:"tool_calls"`
	ToolCallID       *string         `json:"tool_call_id"`
}

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens           *int              `json:"max_tokens"`
	MaxCompletionTokens *int              `json:"max_completion_tokens"`
	Tools               []json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls"`
	Temperature         *float64          `json:"temperature"`
	TopP                *float64          `json:"top_p"`
	FrequencyPenalty    *float64          `json:"frequency_penalty"`
	PresencePenalty     *float64          `json:"presence_penalty"`
	Seed                json.RawMessage   `json:"seed"`
	Stop                json.RawMessage   `json:"stop"`
	N                   *int              `json:"n"`
	ResponseFormat      json.RawMessage   `json:"response_format"`
	ServiceTier         *string           `json:"service_tier"`
}

type contentPart struct {
	Type     string  `json:"type"`
	Text     *string `json:"text"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	} `json:"image_url"`
}

// parseTextParts extracts plain text and image URLs from a content payload.
// allowImages controls whether image_url parts are accepted (current user turn
// only); history turns are text-only. Missing content coerces to empty text,
// matching explicit null: assistant tool-call turns commonly omit content or
// send null. Unknown part fields are ignored for compatibility; structural
// mismatches still fail.

func parseTextParts(raw json.RawMessage, allowImages bool) (string, []string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, true
	}
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return "", nil, false
		}
		return text, nil, true
	}
	if len(raw) == 0 || raw[0] != '[' {
		return "", nil, false
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 || len(parts) > 32 {
		return "", nil, false
	}
	var text strings.Builder
	var images []string
	for _, part := range parts {
		switch part.Type {
		case "text":
			if part.Text == nil || part.ImageURL != nil {
				return "", nil, false
			}
			text.WriteString(*part.Text)
		case "image_url":
			if !allowImages || part.ImageURL == nil || part.Text != nil {
				return "", nil, false
			}
			url := strings.TrimSpace(part.ImageURL.URL)
			if url == "" || len(url) > (8<<20) {
				return "", nil, false
			}
			images = append(images, url)
		default:
			return "", nil, false
		}
	}
	return text.String(), images, true
}

func parseClientTools(raw []json.RawMessage) ([]llm.Tool, bool) {
	if len(raw) > 64 {
		return nil, false
	}
	tools := make([]llm.Tool, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		if len(item) == 0 || len(bytes.TrimSpace(item)) == 0 || len(item) > 64<<10 {
			return nil, false
		}
		// Permissive envelope: unknown fields such as "strict" are ignored
		// for compatibility with evolving client SDKs.
		var wire struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(item, &wire) != nil {
			return nil, false
		}
		if wire.Type != "function" {
			return nil, false
		}
		name := strings.TrimSpace(wire.Function.Name)
		if !clientToolNameRE.MatchString(name) || seen[name] {
			return nil, false
		}
		seen[name] = true
		description := strings.TrimSpace(wire.Function.Description)
		if len([]rune(description)) > 16000 || strings.ContainsRune(description, '\x00') {
			return nil, false
		}
		params := bytes.TrimSpace(wire.Function.Parameters)
		var parsed llm.ToolParameters
		if len(params) == 0 || string(params) == "null" {
			parsed = llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{}}
		} else {
			// Permissive schema: unknown JSON-Schema keywords are dropped
			// rather than rejecting the whole tool.
			if json.Unmarshal(params, &parsed) != nil || parsed.Type != "object" {
				return nil, false
			}
			if parsed.Properties == nil {
				parsed.Properties = map[string]llm.ToolParameterProperty{}
			}
		}
		tools = append(tools, llm.Tool{Type: "function", Function: llm.ToolDefinition{Name: name, Description: description, Parameters: parsed}})
	}
	return tools, true
}

func parseToolChoice(raw json.RawMessage) (agent.ClientToolChoice, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return agent.ClientToolChoice{}, true
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var choice string
		if json.Unmarshal(trimmed, &choice) != nil {
			return agent.ClientToolChoice{}, false
		}
		switch choice {
		case "", "auto":
			return agent.ClientToolChoice{Mode: "auto"}, true
		case "none", "required":
			return agent.ClientToolChoice{Mode: choice}, true
		}
		return agent.ClientToolChoice{}, false
	}
	var typeProbe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(trimmed, &typeProbe) != nil {
		return agent.ClientToolChoice{}, false
	}
	switch typeProbe.Type {
	case "function":
		var wire struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(trimmed, &wire) != nil || wire.Type != "function" || !clientToolNameRE.MatchString(strings.TrimSpace(wire.Function.Name)) {
			return agent.ClientToolChoice{}, false
		}
		return agent.ClientToolChoice{Mode: "named", Named: strings.TrimSpace(wire.Function.Name)}, true
	case "allowed_tools":
		var wire struct {
			Type         string `json:"type"`
			AllowedTools struct {
				Mode  string            `json:"mode"`
				Tools []json.RawMessage `json:"tools"`
			} `json:"allowed_tools"`
		}
		if json.Unmarshal(trimmed, &wire) != nil || wire.Type != "allowed_tools" {
			return agent.ClientToolChoice{}, false
		}
		if wire.AllowedTools.Mode != "auto" && wire.AllowedTools.Mode != "required" {
			return agent.ClientToolChoice{}, false
		}
		if len(wire.AllowedTools.Tools) == 0 || len(wire.AllowedTools.Tools) > 64 {
			return agent.ClientToolChoice{}, false
		}
		names := make([]string, 0, len(wire.AllowedTools.Tools))
		seen := make(map[string]bool, len(wire.AllowedTools.Tools))
		for _, item := range wire.AllowedTools.Tools {
			if len(item) == 0 || len(item) > 4<<10 {
				return agent.ClientToolChoice{}, false
			}
			var entry struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(item, &entry) != nil || entry.Type != "function" || !clientToolNameRE.MatchString(strings.TrimSpace(entry.Function.Name)) {
				return agent.ClientToolChoice{}, false
			}
			name := strings.TrimSpace(entry.Function.Name)
			if seen[name] {
				return agent.ClientToolChoice{}, false
			}
			seen[name] = true
			names = append(names, name)
		}
		return agent.ClientToolChoice{Mode: "allowed", Allowed: names, AllowedMode: wire.AllowedTools.Mode}, true
	default:
		return agent.ClientToolChoice{}, false
	}
}

// parseStopSequences normalizes the OpenAI stop field (a string or an array
// of up to four strings) into a sequence list. Null/missing means no stops;
// empty sequences are dropped, and an all-empty input means no stops.

func parseStopSequences(raw json.RawMessage) ([]string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, true
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var sequence string
		if json.Unmarshal(trimmed, &sequence) != nil || len(sequence) > 4096 {
			return nil, false
		}
		if sequence == "" {
			return nil, true
		}
		return []string{sequence}, true
	}
	var sequences []string
	if json.Unmarshal(trimmed, &sequences) != nil || len(sequences) > 4 {
		return nil, false
	}
	kept := sequences[:0]
	for _, sequence := range sequences {
		if len(sequence) > 4096 {
			return nil, false
		}
		if sequence != "" {
			kept = append(kept, sequence)
		}
	}
	if len(kept) == 0 {
		return nil, true
	}
	return kept, true
}

// parseResponseFormat normalizes the OpenAI response_format field. Only the
// text and json_object shapes are supported; json_schema needs output-schema
// enforcement the provider loop does not implement, so it fails here.

func parseResponseFormat(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", true
	}
	var wire struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(trimmed, &wire) != nil {
		return "", false
	}
	switch wire.Type {
	case "text":
		return "", true
	case "json_object":
		return "json_object", true
	default:
		return "", false
	}
}

// parseSeed normalizes the OpenAI seed field to an integer. Floats and
// out-of-range values fail as unsupported options rather than malformed
// JSON: the envelope is valid, only the value is unusable.

func parseSeed(raw json.RawMessage) (*int64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, true
	}
	var seed int64
	if json.Unmarshal(trimmed, &seed) != nil {
		return nil, false
	}
	// Reject floats such as 7.5, which decode into int64 targets only by
	// truncation on some parsers; encoding/json rejects them outright, but
	// re-marshaling guards against exponential forms losing precision.
	var check float64
	if json.Unmarshal(trimmed, &check) != nil || check != math.Trunc(check) {
		return nil, false
	}
	return &seed, true
}

func inRange(value *float64, min, max float64) bool {
	return value == nil || (!math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= min && *value <= max)
}

func parseWireToolCalls(calls []wireToolCall) ([]llm.ToolCall, bool) {
	if len(calls) == 0 {
		return nil, true
	}
	if len(calls) > 16 {
		return nil, false
	}
	out := make([]llm.ToolCall, 0, len(calls))
	seen := map[string]bool{}
	for _, call := range calls {
		id := strings.TrimSpace(call.ID)
		if id == "" || len(id) > 128 || seen[id] {
			return nil, false
		}
		seen[id] = true
		if call.Type != "" && call.Type != "function" {
			return nil, false
		}
		name := strings.TrimSpace(call.Function.Name)
		if !clientToolNameRE.MatchString(name) {
			return nil, false
		}
		argsRaw := strings.TrimSpace(call.Function.Arguments)
		if argsRaw == "" {
			argsRaw = "{}"
		}
		if len(argsRaw) > 32<<10 {
			return nil, false
		}
		var args map[string]interface{}
		decoder := json.NewDecoder(strings.NewReader(argsRaw))
		decoder.UseNumber()
		if decoder.Decode(&args) != nil {
			return nil, false
		}
		if args == nil {
			args = map[string]interface{}{}
		}
		out = append(out, llm.ToolCall{ID: id, Function: llm.ToolFunction{Name: name, Arguments: args, RawArguments: argsRaw}})
	}
	return out, true
}

// fetchImageURL converts one image_url value to a normalized model input.
// Data URLs decode locally; remote URLs fetch with bounded size and time.

func fetchImageURL(ctx context.Context, url string) (llm.InputImage, bool) {
	url = strings.TrimSpace(url)
	if strings.HasPrefix(url, "data:") {
		comma := strings.Index(url, ",")
		if comma < 0 {
			return llm.InputImage{}, false
		}
		header, payload := url[:comma], url[comma+1:]
		if !strings.Contains(header, ";base64") {
			return llm.InputImage{}, false
		}
		mime := strings.TrimPrefix(header, "data:")
		if idx := strings.Index(mime, ";"); idx >= 0 {
			mime = mime[:idx]
		}
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
		if err != nil {
			if data, err = base64.URLEncoding.DecodeString(strings.TrimSpace(payload)); err != nil {
				return llm.InputImage{}, false
			}
		}
		if len(data) == 0 || len(data) > media.MaxImageBytes {
			return llm.InputImage{}, false
		}
		result, err := media.NormalizeInputImageFromBytes(nil, mime, data, "api")
		if err != nil {
			return llm.InputImage{}, false
		}
		return result.Image, true
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return llm.InputImage{}, false
	}
	if err := ctx.Err(); err != nil {
		return llm.InputImage{}, false
	}
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return llm.InputImage{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return llm.InputImage{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return llm.InputImage{}, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil || len(data) == 0 || len(data) > media.MaxImageBytes {
		return llm.InputImage{}, false
	}
	result, err := media.NormalizeInputImageFromBytes(resp.Header, "", data, "api")
	if err != nil {
		return llm.InputImage{}, false
	}
	return result.Image, true
}

const toolContinuationPrompt = "Continue with the completed tool results above to answer the user's last question."
