package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
func (c *Client) Generate(ctx context.Context, req llm.Request, streamCallback func(string)) (*llm.Response, error) {
	endpoint := fmt.Sprintf("%s/api/generate", c.BaseURL)

	// Map generic request to Ollama's specific JSON struct
	ollamaReq := GenerateRequest{
		Model:  req.Model,
		Prompt: req.Prompt,
		System: req.System,
		Format: req.Format,
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

	var finalResponse llm.Response
	finalResponse.Model = req.Model

	// Handle Streaming
	if req.Stream && streamCallback != nil {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var chunk GenerateResponse
			if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
				continue
			}

			// Fire the callback to the websocket
			streamCallback(chunk.Response)

			// Append to the final string
			finalResponse.Response += chunk.Response

			// Accumulate thinking content and grab metrics on the final chunk
			finalResponse.Thinking += chunk.Thinking
			if chunk.Done {
				finalResponse.TotalDuration = chunk.TotalDuration
				finalResponse.EvalDuration = chunk.EvalDuration
				finalResponse.EvalCount = chunk.EvalCount
				finalResponse.PromptEvalDuration = chunk.PromptEvalDuration
			}
		}
		// Thinking models emit content in Thinking and leave Response empty.
		// Promote Thinking to Response so all callers see output transparently.
		if finalResponse.Response == "" && finalResponse.Thinking != "" {
			finalResponse.Response = finalResponse.Thinking
		}
		return &finalResponse, nil
	}

	// Handle Non-Streaming
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var ollamaResp GenerateResponse
	if err := json.Unmarshal(rawBody, &ollamaResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Map Ollama's response back to the generic standard response.
	// Thinking models emit content in Thinking and leave Response empty —
	// promote Thinking to Response so all callers see output transparently.
	response := ollamaResp.Response
	if response == "" && ollamaResp.Thinking != "" {
		response = ollamaResp.Thinking
	}

	return &llm.Response{
		Model:              ollamaResp.Model,
		Response:           response,
		Thinking:           ollamaResp.Thinking,
		TotalDuration:      ollamaResp.TotalDuration,
		LoadDuration:       ollamaResp.LoadDuration,
		PromptEvalDuration: ollamaResp.PromptEvalDuration,
		EvalDuration:       ollamaResp.EvalDuration,
		EvalCount:          ollamaResp.EvalCount,
	}, nil
}
