package ollama

import "context"

// ToolFunction holds the name and arguments of a single tool invocation.
type ToolFunction struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// ToolCall represents a single tool call emitted by the model.
type ToolCall struct {
	Function ToolFunction `json:"function"`
}

// InputImage is a validated image payload attached to a user request.
// Data must contain the base64-encoded image bytes expected by Ollama.
type InputImage struct {
	MimeType string `json:"mime_type,omitempty"`
	Data     string `json:"data"`
	Source   string `json:"source,omitempty"`
}

// ChatMessage is a single turn in a conversation (system, user, assistant, or tool).
type ChatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Images    []string   `json:"images,omitempty"`
	Thinking  string     `json:"thinking,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
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

// ChatRequest is the payload for a chat-style Ollama request.
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
	PromptEvalCount    int
	DoneReason         string
	TotalDuration      int64
	LoadDuration       int64
	PromptEvalDuration int64
	EvalDuration       int64
	EvalCount          int
}

// ShowRequest requests model metadata from Ollama's /api/show endpoint.
type ShowRequest struct {
	Model   string `json:"model"`
	Verbose bool   `json:"verbose,omitempty"`
}

// ShowResponse contains model metadata returned by Ollama's /api/show endpoint.
type ShowResponse struct {
	Parameters   string                 `json:"parameters,omitempty"`
	Capabilities []string               `json:"capabilities,omitempty"`
	ModelInfo    map[string]interface{} `json:"model_info,omitempty"`
}

// Chatter describes the single Ollama capability the agent depends on.
type Chatter interface {
	Chat(ctx context.Context, req ChatRequest, chatStreamCallback func(chunk ChatMessage)) (*ChatResponse, error)
}
