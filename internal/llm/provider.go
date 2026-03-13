package llm

import "context"

// Request represents the generic payload sent to any LLM provider.
type Request struct {
	Model  string
	Prompt string
	System string
	Format string
	Stream bool
}

// Response represents a standardized reply from any LLM provider.
type Response struct {
	Model              string
	Response           string
	TotalDuration      int64
	LoadDuration       int64
	PromptEvalDuration int64
	EvalDuration       int64
	EvalCount          int
}

// Provider defines the standard methods all LLM clients must implement.
type Provider interface {
	Generate(ctx context.Context, req Request, streamCallback func(chunk string)) (*Response, error)
}
