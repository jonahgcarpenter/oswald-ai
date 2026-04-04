package ollama

import "time"

// ollamaToolFunction holds the name and arguments of a tool invocation from the model.
// Differs from ToolFunction in that Arguments is map[string]interface{} for lazy decoding.
type ollamaToolFunction struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// ollamaToolCall represents a single tool invocation in Ollama's chat response.
// Corresponds to ToolCall but uses Ollama's internal structures.
type ollamaToolCall struct {
	Function ollamaToolFunction `json:"function"`
}

// ollamaMessage is a single conversation turn in Ollama's chat API format.
// mapToOllamaMessages and mapFromOllamaMessage handle the conversion.
type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

// ollamaToolParameterProperty describes a single property in a tool's parameter schema.
type ollamaToolParameterProperty struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// ollamaToolParameters is the JSON Schema definition for a tool's input parameters.
// Mirrors ToolParameters but uses Ollama's exact field structure.
type ollamaToolParameters struct {
	Type       string                                 `json:"type"`
	Properties map[string]ollamaToolParameterProperty `json:"properties"`
	Required   []string                               `json:"required,omitempty"`
}

// ollamaToolDefinition holds the schema for a single function tool.
// Corresponds to ToolDefinition but uses Ollama's internal structures.
type ollamaToolDefinition struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Parameters  ollamaToolParameters `json:"parameters"`
}

// ollamaTool wraps an ollamaToolDefinition with its type.
type ollamaTool struct {
	Type     string               `json:"type"`
	Function ollamaToolDefinition `json:"function"`
}

// ollamaChatRequest represents the payload sent to Ollama's /api/chat endpoint.
type ollamaChatRequest struct {
	Model    string                 `json:"model"`
	Messages []ollamaMessage        `json:"messages"`
	Tools    []ollamaTool           `json:"tools,omitempty"`
	Format   string                 `json:"format,omitempty"`
	Stream   bool                   `json:"stream"`
	Options  map[string]interface{} `json:"options,omitempty"`
}

// ollamaChatResponse represents Ollama's reply from /api/chat.
type ollamaChatResponse struct {
	Model              string        `json:"model"`
	CreatedAt          time.Time     `json:"created_at"`
	Message            ollamaMessage `json:"message"`
	Done               bool          `json:"done"`
	DoneReason         string        `json:"done_reason,omitempty"`
	TotalDuration      int64         `json:"total_duration,omitempty"`
	LoadDuration       int64         `json:"load_duration,omitempty"`
	PromptEvalCount    int           `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64         `json:"prompt_eval_duration,omitempty"`
	EvalCount          int           `json:"eval_count,omitempty"`
	EvalDuration       int64         `json:"eval_duration,omitempty"`
}
