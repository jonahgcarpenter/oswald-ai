package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// Client interacts with the local Ollama REST API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 2 * time.Minute,
		},
	}
}

// Generate now satisfies the llm.Provider interface
func (c *Client) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	endpoint := fmt.Sprintf("%s/api/generate", c.BaseURL)

	// Map generic request to Ollama's specific JSON struct
	ollamaReq := GenerateRequest{
		Model:  req.Model,
		Prompt: req.Prompt,
		System: req.System,
		Stream: req.Stream,
	}

	payloadBytes, err := json.Marshal(ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("Failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("Failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Ollama API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Ollama returned non-200 status: %d", resp.StatusCode)
	}

	var ollamaResp GenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, fmt.Errorf("Failed to decode response: %w", err)
	}

	// Map Ollama's response back to the generic standard response
	return &llm.Response{
		Model:              ollamaResp.Model,
		Response:           ollamaResp.Response,
		TotalDuration:      ollamaResp.TotalDuration,
		LoadDuration:       ollamaResp.LoadDuration,
		PromptEvalDuration: ollamaResp.PromptEvalDuration,
		EvalDuration:       ollamaResp.EvalDuration,
		EvalCount:          ollamaResp.EvalCount,
	}, nil
}

