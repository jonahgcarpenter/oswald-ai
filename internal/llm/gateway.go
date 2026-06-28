package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

// GatewayClient interacts with the LLM gateway's OpenAI-compatible REST API.
type GatewayClient struct {
	BaseURL    string
	APIKey     string
	VirtualKey string
	HTTPClient *http.Client
	log        *config.Logger
}

// NewGatewayClient creates an LLM gateway client with the given base URL, optional auth, timeout, and logger.
func NewGatewayClient(baseURL, apiKey, virtualKey string, timeout time.Duration, log *config.Logger) *GatewayClient {
	return &GatewayClient{
		BaseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:     strings.TrimSpace(apiKey),
		VirtualKey: strings.TrimSpace(virtualKey),
		HTTPClient: &http.Client{
			Timeout: timeout,
		},
		log: log,
	}
}

func (c *GatewayClient) applyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.VirtualKey != "" {
		req.Header.Set("x-bf-vk", c.VirtualKey)
	}
}

func encodeToolArguments(args map[string]interface{}) string {
	if len(args) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func decodeToolArguments(raw string) map[string]interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]interface{}{}
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return map[string]interface{}{"_raw": raw}
	}
	return args
}

func mapToGatewayMessages(msgs []ChatMessage) []gatewayMessage {
	result := make([]gatewayMessage, len(msgs))
	for i, m := range msgs {
		bm := gatewayMessage{Role: m.Role}
		if m.Role == "tool" {
			bm.ToolCallID = m.ToolCallID
			bm.Content = m.Content
			result[i] = bm
			continue
		}
		if len(m.Images) > 0 {
			parts := make([]gatewayContentPart, 0, 1+len(m.Images))
			if strings.TrimSpace(m.Content) != "" {
				parts = append(parts, gatewayContentPart{Type: "text", Text: m.Content})
			}
			for _, image := range m.Images {
				mimeType := strings.TrimSpace(image.MimeType)
				if mimeType == "" {
					mimeType = "image/jpeg"
				}
				parts = append(parts, gatewayContentPart{Type: "image_url", ImageURL: &gatewayImageURL{URL: fmt.Sprintf("data:%s;base64,%s", mimeType, image.Data)}})
			}
			bm.Content = parts
		} else {
			bm.Content = m.Content
		}
		if len(m.ToolCalls) > 0 {
			bm.ToolCalls = make([]gatewayToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				bm.ToolCalls[j] = gatewayToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: gatewayToolFunction{
						Name:      tc.Function.Name,
						Arguments: encodeToolArguments(tc.Function.Arguments),
					},
				}
			}
		}
		result[i] = bm
	}
	return result
}

func contentToString(content interface{}) string {
	switch value := content.(type) {
	case string:
		return value
	case []interface{}:
		var parts []string
		for _, raw := range value {
			obj, ok := raw.(map[string]interface{})
			if !ok || obj["type"] != "text" {
				continue
			}
			if text, ok := obj["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}

func mapFromGatewayMessage(m gatewayMessage) ChatMessage {
	msg := ChatMessage{
		Role:       m.Role,
		Content:    contentToString(m.Content),
		Thinking:   firstNonEmpty(m.ReasoningContent, m.Thinking),
		ToolCallID: m.ToolCallID,
	}
	if len(m.ToolCalls) > 0 {
		msg.ToolCalls = make([]ToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			msg.ToolCalls[i] = ToolCall{
				ID: tc.ID,
				Function: ToolFunction{
					Name:      tc.Function.Name,
					Arguments: decodeToolArguments(tc.Function.Arguments),
				},
			}
		}
	}
	return msg
}

func responseFormat(format string) *gatewayResponseFormat {
	format = strings.TrimSpace(format)
	if format == "" {
		return nil
	}
	if strings.EqualFold(format, "json") {
		format = "json_object"
	}
	return &gatewayResponseFormat{Type: format}
}

func bodySnippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	text = strings.Join(strings.Fields(text), " ")
	const maxBodySnippetChars = 500
	if len(text) > maxBodySnippetChars {
		text = text[:maxBodySnippetChars]
	}
	return text
}

func firstChoice(resp gatewayChatResponse) (gatewayChoice, bool) {
	if len(resp.Choices) == 0 {
		return gatewayChoice{}, false
	}
	return resp.Choices[0], true
}

// Embed sends text to the LLM gateway's /v1/embeddings endpoint and returns vectors.
func (c *GatewayClient) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error) {
	endpoint := fmt.Sprintf("%s/v1/embeddings", c.BaseURL)
	payloadBytes, err := json.Marshal(gatewayEmbeddingRequest{Model: req.Model, Input: req.Input})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embedding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create embedding request: %w", err)
	}
	c.applyHeaders(httpReq)

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("LLM gateway embedding API request failed: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read embedding response body: %w", err)
	}

	meta := requestctx.MetadataFromContext(ctx)
	requestLog := c.log.With(config.F("request_id", meta.RequestID), config.F("gateway", meta.Gateway), config.F("user_id", meta.SenderID), config.F("session_id", meta.SessionID), config.F("model", req.Model))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := bodySnippet(rawBody)
		requestLog.Error("provider.gateway.embed.http_error", "LLM gateway embed returned non-2xx", config.F("operation", "embed"), config.F("http_status", resp.StatusCode), config.F("error_body", snippet))
		if snippet != "" {
			return nil, fmt.Errorf("LLM gateway embed returned HTTP %d: %s", resp.StatusCode, snippet)
		}
		return nil, fmt.Errorf("LLM gateway embed returned HTTP %d", resp.StatusCode)
	}

	var gatewayResp gatewayEmbeddingResponse
	if err := json.Unmarshal(rawBody, &gatewayResp); err != nil {
		requestLog.Error("provider.gateway.embed.decode_error", "failed to decode LLM gateway embed response", config.F("operation", "embed"), config.ErrorField(err))
		return nil, fmt.Errorf("failed to decode embedding response: %w", err)
	}
	if gatewayResp.Error != nil && gatewayResp.Error.Message != "" {
		return nil, fmt.Errorf("LLM gateway embed error: %s", gatewayResp.Error.Message)
	}
	if len(gatewayResp.Data) == 0 || len(gatewayResp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("LLM gateway embed response contained no embeddings")
	}

	embeddings := make([][]float64, 0, len(gatewayResp.Data))
	for _, datum := range gatewayResp.Data {
		embeddings = append(embeddings, datum.Embedding)
	}
	return &EmbedResponse{Model: gatewayResp.Model, Embeddings: embeddings}, nil
}

// Chat sends a multi-turn conversation to the LLM gateway's /v1/chat/completions endpoint.
func (c *GatewayClient) Chat(ctx context.Context, req ChatRequest, chatStreamCallback func(chunk ChatMessage)) (*ChatResponse, error) {
	endpoint := fmt.Sprintf("%s/v1/chat/completions", c.BaseURL)
	gatewayReq := gatewayChatRequest{Model: req.Model, User: req.User, Messages: mapToGatewayMessages(req.Messages), Tools: req.Tools, ResponseFormat: responseFormat(req.Format), Stream: req.Stream}
	payloadBytes, err := json.Marshal(gatewayReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal chat request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create chat request: %w", err)
	}
	c.applyHeaders(httpReq)

	startedAt := time.Now()
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("LLM gateway chat API request failed: %w", err)
	}
	defer resp.Body.Close()

	if req.Stream && chatStreamCallback != nil {
		return c.readChatStream(ctx, resp, req.Model, startedAt, chatStreamCallback)
	}
	return c.readChatResponse(ctx, resp, req.Model, startedAt)
}

func (c *GatewayClient) requestLog(ctx context.Context, model string) *config.Logger {
	meta := requestctx.MetadataFromContext(ctx)
	return c.log.With(config.F("request_id", meta.RequestID), config.F("gateway", meta.Gateway), config.F("user_id", meta.SenderID), config.F("session_id", meta.SessionID), config.F("model", model))
}

func (c *GatewayClient) readChatResponse(ctx context.Context, resp *http.Response, model string, startedAt time.Time) (*ChatResponse, error) {
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read chat response body: %w", err)
	}
	requestLog := c.requestLog(ctx, model)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := bodySnippet(rawBody)
		requestLog.Error("provider.gateway.chat.http_error", "LLM gateway chat returned non-2xx", config.F("operation", "chat"), config.F("http_status", resp.StatusCode), config.F("error_body", snippet))
		if snippet != "" {
			return nil, fmt.Errorf("LLM gateway chat returned HTTP %d: %s", resp.StatusCode, snippet)
		}
		return nil, fmt.Errorf("LLM gateway chat returned HTTP %d", resp.StatusCode)
	}

	var gatewayResp gatewayChatResponse
	if err := json.Unmarshal(rawBody, &gatewayResp); err != nil {
		requestLog.Error("provider.gateway.chat.decode_error", "failed to decode LLM gateway chat response", config.F("operation", "chat"), config.ErrorField(err))
		return nil, fmt.Errorf("failed to decode chat response: %w", err)
	}
	if gatewayResp.Error != nil && gatewayResp.Error.Message != "" {
		return nil, fmt.Errorf("LLM gateway chat error: %s", gatewayResp.Error.Message)
	}
	choice, ok := firstChoice(gatewayResp)
	if !ok {
		return nil, fmt.Errorf("LLM gateway chat response contained no choices")
	}
	msg := mapFromGatewayMessage(choice.Message)
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	durationMS := time.Since(startedAt).Milliseconds()
	requestLog.Info("provider.gateway.chat.complete", "LLM gateway chat completed",
		config.F("operation", "chat"),
		config.F("duration_ms", durationMS),
		config.F("prompt_tokens", gatewayResp.Usage.PromptTokens),
		config.F("completion_tokens", gatewayResp.Usage.CompletionTokens),
		config.F("total_tokens", gatewayResp.Usage.TotalTokens),
		config.F("is_usage_reported", usageReported(gatewayResp.Usage.PromptTokens, gatewayResp.Usage.CompletionTokens, gatewayResp.Usage.TotalTokens)),
		config.F("done_reason", choice.FinishReason),
		config.F("is_streaming", false),
		config.F("status", "ok"),
	)

	return &ChatResponse{
		Model:            firstNonEmpty(gatewayResp.Model, model),
		Message:          msg,
		PromptTokens:     gatewayResp.Usage.PromptTokens,
		CompletionTokens: gatewayResp.Usage.CompletionTokens,
		TotalTokens:      gatewayResp.Usage.TotalTokens,
		DurationMS:       durationMS,
		DoneReason:       choice.FinishReason,
	}, nil
}

func (c *GatewayClient) readChatStream(ctx context.Context, resp *http.Response, model string, startedAt time.Time, chatStreamCallback func(chunk ChatMessage)) (*ChatResponse, error) {
	requestLog := c.requestLog(ctx, model)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("failed to read chat stream response body: %w", readErr)
		}
		snippet := bodySnippet(rawBody)
		requestLog.Error("provider.gateway.chat.http_error", "LLM gateway chat stream returned non-2xx", config.F("operation", "chat_stream"), config.F("http_status", resp.StatusCode), config.F("error_body", snippet))
		if snippet != "" {
			return nil, fmt.Errorf("LLM gateway chat stream returned HTTP %d: %s", resp.StatusCode, snippet)
		}
		return nil, fmt.Errorf("LLM gateway chat stream returned HTTP %d", resp.StatusCode)
	}

	var final ChatResponse
	final.Model = model
	toolParts := map[int]*gatewayStreamToolCall{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk gatewayChatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			requestLog.Warn("provider.gateway.chat.stream.parse_failed", "failed to parse LLM gateway chat stream chunk", config.F("operation", "chat_stream"), config.F("status", "degraded"), config.ErrorField(err))
			continue
		}
		if chunk.Model != "" {
			final.Model = chunk.Model
		}
		choice, ok := firstChoice(chunk)
		if !ok {
			continue
		}
		final.DoneReason = choice.FinishReason
		content := contentToString(choice.Delta.Content)
		thinking := firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Thinking)
		if thinking != "" {
			final.Message.Role = "assistant"
			final.Message.Thinking += thinking
			chatStreamCallback(ChatMessage{Role: "assistant", Thinking: thinking})
		}
		if content != "" {
			final.Message.Role = "assistant"
			final.Message.Content += content
			chatStreamCallback(ChatMessage{Role: "assistant", Content: content})
		}
		for _, tc := range choice.Delta.ToolCalls {
			idx := tc.Index
			part, ok := toolParts[idx]
			if !ok {
				part = &gatewayStreamToolCall{}
				toolParts[idx] = part
			}
			if tc.ID != "" {
				part.ID = tc.ID
			}
			if tc.Function.Name != "" {
				part.Name = tc.Function.Name
			}
			part.Arguments += tc.Function.Arguments
		}
	}
	if err := scanner.Err(); err != nil {
		requestLog.Warn("provider.gateway.chat.stream.scan_failed", "LLM gateway chat stream scan failed", config.F("operation", "chat_stream"), config.F("status", "degraded"), config.ErrorField(err))
	}

	if len(toolParts) > 0 {
		final.Message.Role = "assistant"
		for i := 0; i < len(toolParts); i++ {
			part := toolParts[i]
			if part == nil || part.Name == "" {
				continue
			}
			final.Message.ToolCalls = append(final.Message.ToolCalls, ToolCall{ID: firstNonEmpty(part.ID, fmt.Sprintf("call_stream_%d", i+1)), Function: ToolFunction{Name: part.Name, Arguments: decodeToolArguments(part.Arguments)}})
		}
	}
	final.DurationMS = time.Since(startedAt).Milliseconds()
	requestLog.Info("provider.gateway.chat.complete", "LLM gateway chat completed",
		config.F("operation", "chat_stream"),
		config.F("duration_ms", final.DurationMS),
		config.F("prompt_tokens", final.PromptTokens),
		config.F("completion_tokens", final.CompletionTokens),
		config.F("total_tokens", final.TotalTokens),
		config.F("is_usage_reported", usageReported(final.PromptTokens, final.CompletionTokens, final.TotalTokens)),
		config.F("done_reason", final.DoneReason),
		config.F("is_streaming", true),
		config.F("status", "ok"),
	)
	return &final, nil
}

func usageReported(promptTokens, completionTokens, totalTokens int) bool {
	return promptTokens > 0 || completionTokens > 0 || totalTokens > 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
