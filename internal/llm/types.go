package llm

type bifrostToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type bifrostToolCall struct {
	ID       string              `json:"id,omitempty"`
	Index    int                 `json:"index,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function bifrostToolFunction `json:"function"`
}

type bifrostImageURL struct {
	URL string `json:"url"`
}

type bifrostContentPart struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	ImageURL *bifrostImageURL `json:"image_url,omitempty"`
}

type bifrostMessage struct {
	Role             string            `json:"role"`
	Content          interface{}       `json:"content,omitempty"`
	Thinking         string            `json:"thinking,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []bifrostToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
}

type bifrostResponseFormat struct {
	Type string `json:"type"`
}

type bifrostChatRequest struct {
	Model          string                 `json:"model"`
	User           string                 `json:"user,omitempty"`
	Messages       []bifrostMessage       `json:"messages"`
	Tools          []Tool                 `json:"tools,omitempty"`
	ResponseFormat *bifrostResponseFormat `json:"response_format,omitempty"`
	Stream         bool                   `json:"stream"`
}

type bifrostChatResponse struct {
	ID      string                `json:"id,omitempty"`
	Model   string                `json:"model,omitempty"`
	Choices []bifrostChoice       `json:"choices"`
	Usage   bifrostUsage          `json:"usage,omitempty"`
	Error   *bifrostErrorResponse `json:"error,omitempty"`
}

type bifrostChoice struct {
	Index        int            `json:"index,omitempty"`
	Message      bifrostMessage `json:"message,omitempty"`
	Delta        bifrostMessage `json:"delta,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
}

type bifrostUsage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

type bifrostErrorResponse struct {
	Message string `json:"message,omitempty"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

type bifrostStreamToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type bifrostEmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type bifrostEmbeddingResponse struct {
	Model string                  `json:"model,omitempty"`
	Data  []bifrostEmbeddingDatum `json:"data"`
	Error *bifrostErrorResponse   `json:"error,omitempty"`
}

type bifrostEmbeddingDatum struct {
	Embedding []float64 `json:"embedding"`
}
