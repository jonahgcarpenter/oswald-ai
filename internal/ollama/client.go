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

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Client interacts with the local Ollama REST API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	log        *config.Logger
}

// NewClient creates an Ollama client with the given base URL and logger.
func NewClient(baseURL string, log *config.Logger) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 2 * time.Minute,
		},
		log: log,
	}
}

// Generate sends a single-turn prompt to Ollama's /api/generate endpoint.
// Deprecated: Use Chat instead. This endpoint lacks tool-calling support.
func (c *Client) Generate(ctx context.Context, req Request, streamCallback func(string)) (*Response, error) {
	endpoint := fmt.Sprintf("%s/api/generate", c.BaseURL)

	ollamaReq := GenerateRequest{
		Model:  req.Model,
		Prompt: req.Prompt,
		System: req.System,
		Format: req.Format,
		Stream: req.Stream,
	}

	payloadBytes, err := json.Marshal(ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Ollama API request failed: %w", err)
	}
	defer resp.Body.Close()

	var finalResponse Response
	finalResponse.Model = req.Model

	if req.Stream && streamCallback != nil {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var chunk GenerateResponse
			if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
				c.log.Debug("Ollama stream: failed to parse chunk: %v | raw: %q", err, scanner.Text())
				continue
			}

			streamCallback(chunk.Response)
			finalResponse.Response += chunk.Response
			finalResponse.Thinking += chunk.Thinking

			if chunk.Done {
				finalResponse.TotalDuration = chunk.TotalDuration
				finalResponse.EvalDuration = chunk.EvalDuration
				finalResponse.EvalCount = chunk.EvalCount
				finalResponse.PromptEvalDuration = chunk.PromptEvalDuration
			}
		}
		if err := scanner.Err(); err != nil {
			c.log.Warn("Ollama stream: scanner error: %v", err)
		}

		if finalResponse.Response == "" && finalResponse.Thinking != "" {
			finalResponse.Response = finalResponse.Thinking
		}
		return &finalResponse, nil
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.log.Error("Ollama returned HTTP %d: %s", resp.StatusCode, string(rawBody))
		return nil, fmt.Errorf("Ollama returned HTTP %d", resp.StatusCode)
	}

	var ollamaResp GenerateResponse
	if err := json.Unmarshal(rawBody, &ollamaResp); err != nil {
		c.log.Error("Ollama response decode failed: %v | raw: %q", err, string(rawBody))
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	response := ollamaResp.Response
	if response == "" && ollamaResp.Thinking != "" {
		response = ollamaResp.Thinking
	}

	return &Response{
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

// mapToOllamaMessages converts ChatMessage values to Ollama's internal chat message type.
func mapToOllamaMessages(msgs []ChatMessage) []ollamaMessage {
	result := make([]ollamaMessage, len(msgs))
	for i, m := range msgs {
		cm := ollamaMessage{
			Role:     m.Role,
			Content:  m.Content,
			Thinking: m.Thinking,
			ToolName: m.ToolName,
		}
		if len(m.ToolCalls) > 0 {
			cm.ToolCalls = make([]ollamaToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				cm.ToolCalls[j] = ollamaToolCall{
					Function: ollamaToolFunction{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
			}
		}
		result[i] = cm
	}
	return result
}

// mapToOllamaTools converts Tool values to Ollama's internal tool type.
func mapToOllamaTools(tools []Tool) []ollamaTool {
	result := make([]ollamaTool, len(tools))
	for i, t := range tools {
		props := make(map[string]ollamaToolParameterProperty, len(t.Function.Parameters.Properties))
		for k, v := range t.Function.Parameters.Properties {
			props[k] = ollamaToolParameterProperty{
				Type:        v.Type,
				Description: v.Description,
				Enum:        v.Enum,
			}
		}
		result[i] = ollamaTool{
			Type: t.Type,
			Function: ollamaToolDefinition{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters: ollamaToolParameters{
					Type:       t.Function.Parameters.Type,
					Properties: props,
					Required:   t.Function.Parameters.Required,
				},
			},
		}
	}
	return result
}

// mapFromOllamaMessage converts Ollama's internal chat message to ChatMessage.
func mapFromOllamaMessage(m ollamaMessage) ChatMessage {
	msg := ChatMessage{
		Role:     m.Role,
		Content:  m.Content,
		Thinking: m.Thinking,
		ToolName: m.ToolName,
	}
	if len(m.ToolCalls) > 0 {
		msg.ToolCalls = make([]ToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			msg.ToolCalls[i] = ToolCall{
				Function: ToolFunction{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			}
		}
	}
	return msg
}

// Chat sends a multi-turn conversation to Ollama's /api/chat endpoint.
// Supports tool calling and streaming.
func (c *Client) Chat(ctx context.Context, req ChatRequest, chatStreamCallback func(chunk ChatMessage)) (*ChatResponse, error) {
	endpoint := fmt.Sprintf("%s/api/chat", c.BaseURL)

	ollamaReq := ollamaChatRequest{
		Model:    req.Model,
		Messages: mapToOllamaMessages(req.Messages),
		Tools:    mapToOllamaTools(req.Tools),
		Format:   req.Format,
		Stream:   req.Stream,
	}

	payloadBytes, err := json.Marshal(ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal chat request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Ollama chat API request failed: %w", err)
	}
	defer resp.Body.Close()

	if req.Stream && chatStreamCallback != nil {
		var finalResp ChatResponse
		finalResp.Model = req.Model

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		for scanner.Scan() {
			var chunk ollamaChatResponse
			if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
				c.log.Debug("Ollama chat stream: failed to parse chunk: %v | raw: %q", err, scanner.Text())
				continue
			}

			msg := mapFromOllamaMessage(chunk.Message)
			chatStreamCallback(msg)

			finalResp.Message.Role = msg.Role
			finalResp.Message.Content += msg.Content
			finalResp.Message.Thinking += msg.Thinking
			if len(msg.ToolCalls) > 0 {
				finalResp.Message.ToolCalls = append(finalResp.Message.ToolCalls, msg.ToolCalls...)
			}

			if chunk.Done {
				finalResp.DoneReason = chunk.DoneReason
				finalResp.TotalDuration = chunk.TotalDuration
				finalResp.LoadDuration = chunk.LoadDuration
				finalResp.PromptEvalDuration = chunk.PromptEvalDuration
				finalResp.EvalDuration = chunk.EvalDuration
				finalResp.EvalCount = chunk.EvalCount
			}
		}
		if err := scanner.Err(); err != nil {
			c.log.Warn("Ollama chat stream: scanner error: %v", err)
		}

		if finalResp.Message.Content == "" && finalResp.Message.Thinking != "" {
			finalResp.Message.Content = finalResp.Message.Thinking
		}
		return &finalResp, nil
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read chat response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.log.Error("Ollama chat returned HTTP %d: %s", resp.StatusCode, string(rawBody))
		return nil, fmt.Errorf("Ollama chat returned HTTP %d", resp.StatusCode)
	}

	var ollamaResp ollamaChatResponse
	if err := json.Unmarshal(rawBody, &ollamaResp); err != nil {
		c.log.Error("Ollama chat response decode failed: %v | raw: %q", err, string(rawBody))
		return nil, fmt.Errorf("failed to decode chat response: %w", err)
	}

	msg := mapFromOllamaMessage(ollamaResp.Message)
	if msg.Content == "" && msg.Thinking != "" {
		msg.Content = msg.Thinking
	}

	return &ChatResponse{
		Model:              ollamaResp.Model,
		Message:            msg,
		DoneReason:         ollamaResp.DoneReason,
		TotalDuration:      ollamaResp.TotalDuration,
		LoadDuration:       ollamaResp.LoadDuration,
		PromptEvalDuration: ollamaResp.PromptEvalDuration,
		EvalDuration:       ollamaResp.EvalDuration,
		EvalCount:          ollamaResp.EvalCount,
	}, nil
}
