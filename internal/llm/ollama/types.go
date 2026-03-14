package ollama

import "time"

// GenerateRequest represents the payload sent to Ollama's /api/generate endpoint.
// Deprecated: Use ChatRequest with /api/chat instead.
type GenerateRequest struct {
	Model   string                 `json:"model"`
	Prompt  string                 `json:"prompt"`
	Format  string                 `json:"format,omitempty"`
	Stream  bool                   `json:"stream"`
	System  string                 `json:"system,omitempty"`
	Options map[string]interface{} `json:"options,omitempty"`
}

// GenerateResponse represents Ollama's reply from /api/generate.
// Deprecated: Use ChatResponse with /api/chat instead.
type GenerateResponse struct {
	Model              string    `json:"model"`
	CreatedAt          time.Time `json:"created_at"`
	Response           string    `json:"response"`
	Thinking           string    `json:"thinking,omitempty"` // populated by thinking/reasoning models
	Done               bool      `json:"done"`
	DoneReason         string    `json:"done_reason,omitempty"`
	Context            []int     `json:"context,omitempty"`
	TotalDuration      int64     `json:"total_duration,omitempty"`
	LoadDuration       int64     `json:"load_duration,omitempty"`
	PromptEvalCount    int       `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64     `json:"prompt_eval_duration,omitempty"`
	EvalCount          int       `json:"eval_count,omitempty"`
	EvalDuration       int64     `json:"eval_duration,omitempty"`
}

// chatToolFunction holds the name and arguments of a tool call from the model.
type chatToolFunction struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// chatToolCall represents a single tool invocation emitted by the model.
type chatToolCall struct {
	Function chatToolFunction `json:"function"`
}

// chatMessage is a single conversation turn in the Ollama chat API.
type chatMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Thinking  string         `json:"thinking,omitempty"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
}

// chatToolParameterProperty describes one property in a tool's parameter schema.
type chatToolParameterProperty struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// chatToolParameters is the JSON Schema for a tool's inputs.
type chatToolParameters struct {
	Type       string                               `json:"type"`
	Properties map[string]chatToolParameterProperty `json:"properties"`
	Required   []string                             `json:"required,omitempty"`
}

// chatToolDefinition holds the schema for a single function tool.
type chatToolDefinition struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Parameters  chatToolParameters `json:"parameters"`
}

// chatTool wraps a chatToolDefinition with its type identifier.
type chatTool struct {
	Type     string             `json:"type"`
	Function chatToolDefinition `json:"function"`
}

// ChatRequest represents the payload sent to Ollama's /api/chat endpoint.
type ChatRequest struct {
	Model    string                 `json:"model"`
	Messages []chatMessage          `json:"messages"`
	Tools    []chatTool             `json:"tools,omitempty"`
	Format   string                 `json:"format,omitempty"`
	Stream   bool                   `json:"stream"`
	Options  map[string]interface{} `json:"options,omitempty"`
}

// ChatResponse represents Ollama's reply from /api/chat.
type ChatResponse struct {
	Model              string      `json:"model"`
	CreatedAt          time.Time   `json:"created_at"`
	Message            chatMessage `json:"message"`
	Done               bool        `json:"done"`
	DoneReason         string      `json:"done_reason,omitempty"`
	TotalDuration      int64       `json:"total_duration,omitempty"`
	LoadDuration       int64       `json:"load_duration,omitempty"`
	PromptEvalCount    int         `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64       `json:"prompt_eval_duration,omitempty"`
	EvalCount          int         `json:"eval_count,omitempty"`
	EvalDuration       int64       `json:"eval_duration,omitempty"`
}
