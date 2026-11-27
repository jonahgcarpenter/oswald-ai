package api

import (
	"encoding/json"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

type gatewayToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UnmarshalJSON accepts arguments as a JSON string (canonical) or as an
// object (some providers echo parsed args). Objects re-encode to their
// compact JSON form so downstream decode keeps working.
func (f *gatewayToolFunction) UnmarshalJSON(data []byte) error {
	type plain gatewayToolFunction
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.Name = raw.Name
	if len(raw.Arguments) == 0 || string(raw.Arguments) == "null" {
		f.Arguments = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(raw.Arguments, &s); err == nil {
		f.Arguments = s
		return nil
	}
	f.Arguments = string(raw.Arguments)
	var _ plain = plain(*f)
	return nil
}

type gatewayToolCall struct {
	ID       string              `json:"id,omitempty"`
	Index    int                 `json:"index,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function gatewayToolFunction `json:"function"`
}

type gatewayImageURL struct {
	URL string `json:"url"`
}

type gatewayContentPart struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	ImageURL *gatewayImageURL `json:"image_url,omitempty"`
}

type gatewayMessage struct {
	Role             string            `json:"role"`
	Content          interface{}       `json:"content,omitempty"`
	Name             string            `json:"name,omitempty"`
	Reasoning        string            `json:"reasoning,omitempty"`
	Thinking         string            `json:"thinking,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []gatewayToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
}

type gatewayResponseFormat struct {
	Type string `json:"type"`
}

type gatewayChatRequest struct {
	Model               string                 `json:"model"`
	User                string                 `json:"user,omitempty"`
	Messages            []gatewayMessage       `json:"messages"`
	Tools               []llm.Tool             `json:"tools,omitempty"`
	ToolChoice          llm.ToolChoice         `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                  `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64               `json:"temperature,omitempty"`
	TopP                *float64               `json:"top_p,omitempty"`
	FrequencyPenalty    *float64               `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64               `json:"presence_penalty,omitempty"`
	Seed                *int64                 `json:"seed,omitempty"`
	Stop                []string               `json:"stop,omitempty"`
	MaxTokens           *int                   `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                   `json:"max_completion_tokens,omitempty"`
	ResponseFormat      *gatewayResponseFormat `json:"response_format,omitempty"`
	Stream              bool                   `json:"stream"`
	StreamOptions       *gatewayStreamOptions  `json:"stream_options,omitempty"`
}

type gatewayStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type gatewayChatResponse struct {
	ID      string                `json:"id,omitempty"`
	Model   string                `json:"model,omitempty"`
	Choices []gatewayChoice       `json:"choices"`
	Usage   gatewayUsage          `json:"usage,omitempty"`
	Error   *gatewayErrorResponse `json:"error,omitempty"`
}

// UnmarshalJSON normalizes the many error shapes OpenAI-compatible providers
// emit: {"error":{"message":...}}, {"error":"..."}, {"detail":"..."},
// {"message":"..."} — all collapse to Error with Message set.
func (r *gatewayChatResponse) UnmarshalJSON(data []byte) error {
	type plain gatewayChatResponse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*r = gatewayChatResponse(v)
	if r.Error != nil && r.Error.Message != "" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil
	}
	if raw, ok := fields["error"]; ok && len(raw) > 0 && string(raw) != "null" {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			if r.Error == nil {
				r.Error = &gatewayErrorResponse{}
			}
			if r.Error.Message == "" {
				r.Error.Message = s
			}
			return nil
		}
	}
	if r.Error != nil {
		return nil
	}
	for _, key := range []string{"detail", "message"} {
		if raw, ok := fields[key]; ok && len(raw) > 0 && string(raw) != "null" {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && s != "" {
				r.Error = &gatewayErrorResponse{Message: s}
				return nil
			}
		}
	}
	return nil
}

type gatewayChoice struct {
	Index        int            `json:"index,omitempty"`
	Message      gatewayMessage `json:"message,omitempty"`
	Delta        gatewayMessage `json:"delta,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
}

type gatewayUsage struct {
	reported                                          bool
	promptReported, completionReported, totalReported bool
	reasoningReported                                 bool
	PromptTokens                                      int `json:"prompt_tokens,omitempty"`
	CompletionTokens                                  int `json:"completion_tokens,omitempty"`
	TotalTokens                                       int `json:"total_tokens,omitempty"`
	ReasoningTokens                                   int `json:"reasoning_tokens,omitempty"`
}

func (u *gatewayUsage) UnmarshalJSON(data []byte) error {
	type wireUsage gatewayUsage
	var value wireUsage
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*u = gatewayUsage(value)
	for key, reported := range map[string]*bool{"prompt_tokens": &u.promptReported, "completion_tokens": &u.completionReported, "total_tokens": &u.totalReported, "reasoning_tokens": &u.reasoningReported} {
		if raw, ok := fields[key]; ok && string(raw) != "null" {
			*reported = true
			u.reported = true
		}
	}
	// OpenAI-compatible providers may nest reasoning tokens under
	// completion_tokens_details instead of a top-level field. Some nest
	// under output_tokens_details instead.
	var details map[string]json.RawMessage
	for _, key := range []string{"completion_tokens_details", "output_tokens_details"} {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" {
			continue
		}
		if err := json.Unmarshal(raw, &details); err == nil {
			if raw, ok := details["reasoning_tokens"]; ok && string(raw) != "null" {
				_ = json.Unmarshal(raw, &u.ReasoningTokens)
				u.reasoningReported = true
				u.reported = true
			}
		}
	}
	return nil
}

type gatewayErrorResponse struct {
	Message string `json:"message,omitempty"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

// UnmarshalJSON accepts {"message":...} objects or plain "string" errors.
func (e *gatewayErrorResponse) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		e.Message = s
		return nil
	}
	type plain gatewayErrorResponse
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*e = gatewayErrorResponse(v)
	return nil
}

type gatewayStreamToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type gatewayEmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type gatewayEmbeddingResponse struct {
	Usage gatewayUsage            `json:"usage,omitempty"`
	Model string                  `json:"model,omitempty"`
	Data  []gatewayEmbeddingDatum `json:"data"`
	Error *gatewayErrorResponse   `json:"error,omitempty"`
}

type gatewayEmbeddingDatum struct {
	Embedding []float64 `json:"embedding"`
}

type gatewayAsyncJob struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	StatusCode int             `json:"status_code,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      json.RawMessage `json:"error,omitempty"`
}
