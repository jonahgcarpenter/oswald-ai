package llm

import "context"

// Request represents the generic payload sent to any LLM provider.
// Deprecated: Use ChatRequest with the Chat method instead.
type Request struct {
	Model  string
	Prompt string
	System string
	Format string
	Stream bool
}

// Response represents a standardized reply from any LLM provider.
// Deprecated: Use ChatResponse with the Chat method instead.
type Response struct {
	Model              string
	Response           string
	Thinking           string // populated by thinking/reasoning models; may be empty
	TotalDuration      int64
	LoadDuration       int64
	PromptEvalDuration int64
	EvalDuration       int64
	EvalCount          int
}

// ToolFunction holds the name and arguments of a single tool invocation.
type ToolFunction struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// ToolCall represents a single tool call emitted by the model.
type ToolCall struct {
	Function ToolFunction `json:"function"`
}

// ChatMessage is a single turn in a conversation (system, user, assistant, or tool).
type ChatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Thinking  string     `json:"thinking,omitempty"` // populated by reasoning models
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"` // used when role == "tool"
}

// ToolParameterProperty describes a single property within a tool's parameter schema.
type ToolParameterProperty struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// ToolParameters is the JSON Schema object describing a tool's input parameters.
type ToolParameters struct {
	Type       string                           `json:"type"`
	Properties map[string]ToolParameterProperty `json:"properties"`
	Required   []string                         `json:"required,omitempty"`
}

// ToolDefinition holds the schema for a single function tool.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  ToolParameters `json:"parameters"`
}

// Tool wraps a ToolDefinition with its type identifier (always "function").
type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

// ChatRequest is the generic payload for a chat-style LLM request.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []Tool        `json:"tools,omitempty"`
	Format   string        `json:"format,omitempty"`
	Stream   bool          `json:"stream"`
}

// ChatResponse is the standardized reply from a chat LLM call.
type ChatResponse struct {
	Model              string
	Message            ChatMessage
	DoneReason         string
	TotalDuration      int64
	LoadDuration       int64
	PromptEvalDuration int64
	EvalDuration       int64
	EvalCount          int
}

// Provider defines the standard methods all LLM clients must implement.
type Provider interface {
	// Generate sends a single-turn prompt to the model and returns the response.
	// Deprecated: Use Chat instead.
	Generate(ctx context.Context, req Request, streamCallback func(chunk string)) (*Response, error)

	// Chat sends a multi-turn conversation to the model and returns the response.
	// It supports tool calling and streaming via chatStreamCallback.
	Chat(ctx context.Context, req ChatRequest, chatStreamCallback func(chunk ChatMessage)) (*ChatResponse, error)
}
