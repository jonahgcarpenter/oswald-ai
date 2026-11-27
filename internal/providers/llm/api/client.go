package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

const (
	defaultAsyncPollInterval = time.Second
	maxAsyncPollFailures     = 3
	// gatewayUserAgent identifies Oswald on provider-facing chat/model
	// requests. OpenCode Go asks harnesses to identify with a product UA
	// instead of a generic HTTP-library name.
	gatewayUserAgent = "oswald-ai/1.0 (+llm-gateway)"
	// openCodeSessionHeader carries the stable per-conversation session ID
	// OpenCode Go requires for routing and prompt-cache affinity.
	openCodeSessionHeader = "x-opencode-session"
)

// Client interacts with an OpenAI-compatible REST API.
type Client struct {
	BaseURL      string
	APIKey       string
	VirtualKey   string
	AccountID    string
	HTTPClient   *http.Client
	pollInterval time.Duration
	log          *config.Logger
}

// NewClient creates an LLM gateway client with the given base URL and optional auth.
func NewClient(baseURL, apiKey, virtualKey string, log *config.Logger) *Client {
	return &Client{
		BaseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:       strings.TrimSpace(apiKey),
		VirtualKey:   strings.TrimSpace(virtualKey),
		HTTPClient:   &http.Client{},
		pollInterval: defaultAsyncPollInterval,
		log:          log,
	}
}

func (c *Client) applyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(req.Header.Get("User-Agent")) == "" {
		req.Header.Set("User-Agent", gatewayUserAgent)
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.VirtualKey != "" {
		req.Header.Set("x-bf-vk", c.VirtualKey)
	}
	if c.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", c.AccountID)
		req.Header.Set("originator", "oswald")
	}
}

// applySessionHeader attaches the stable per-conversation session ID when
// present. Values with CR/LF are dropped so header injection fails closed.
func applySessionHeader(req *http.Request, sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.ContainsAny(sessionID, "\r\n") {
		return
	}
	req.Header.Set(openCodeSessionHeader, sessionID)
}

// sessionIDFromContext returns the request-scoped session identifier when the
// caller did not set one explicitly on the chat request.
func sessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	sessionID := strings.TrimSpace(requestctx.MetadataFromContext(ctx).SessionID)
	if sessionID == "" || strings.ContainsAny(sessionID, "\r\n") {
		return ""
	}
	return sessionID
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

func mapToGatewayMessages(msgs []llm.ChatMessage) []gatewayMessage {
	result := make([]gatewayMessage, len(msgs))
	callNames := make(map[string]string)
	for i, m := range msgs {
		bm := gatewayMessage{Role: m.Role, Name: m.Name}
		if m.Role == "tool" {
			bm.ToolCallID = m.ToolCallID
			// Wire names identify the correlated invocation, not its display name.
			// This copy never mutates request/history messages.
			bm.Name = callNames[m.ToolCallID]
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
				callNames[tc.ID] = tc.Function.Name
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

func mapFromGatewayMessage(m gatewayMessage) llm.ChatMessage {
	msg := llm.ChatMessage{
		Role:       m.Role,
		Content:    contentToString(m.Content),
		Name:       m.Name,
		Thinking:   firstNonEmpty(m.ReasoningContent, m.Thinking, m.Reasoning),
		ToolCallID: m.ToolCallID,
	}
	if len(m.ToolCalls) > 0 {
		msg.ToolCalls = make([]llm.ToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			msg.ToolCalls[i] = llm.ToolCall{
				ID: tc.ID,
				Function: llm.ToolFunction{
					Name:         tc.Function.Name,
					Arguments:    decodeToolArguments(tc.Function.Arguments),
					RawArguments: tc.Function.Arguments,
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

type asyncHTTPError struct {
	StatusCode int
	Body       string
}

func (e *asyncHTTPError) Error() string {
	return fmt.Sprintf("LLM gateway async request returned HTTP %d", e.StatusCode)
}

func (e *asyncHTTPError) HTTPStatusCode() int { return e.StatusCode }

type embedHTTPError struct{ StatusCode int }

func (e *embedHTTPError) Error() string {
	return fmt.Sprintf("LLM gateway embed returned HTTP %d", e.StatusCode)
}
func (e *embedHTTPError) HTTPStatusCode() int { return e.StatusCode }

func (c *Client) doAsyncRequest(ctx context.Context, method, endpoint string, body []byte, sessionID string) (*http.Response, []byte, error) {
	if m := measurementFromContext(ctx); m != nil {
		m.phase = "submit"
		if method == http.MethodGet {
			m.phase = "poll"
		}
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("create LLM gateway async request: %w", err)
	}
	c.applyHeaders(req)
	applySessionHeader(req, sessionID)
	if m := measurementFromContext(ctx); m != nil && method == http.MethodPost {
		m.submitted = ctx.Err() == nil && req.URL.Host != "" && (req.URL.Scheme == "http" || req.URL.Scheme == "https")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("send LLM gateway async request: %w", err)
	}
	if m := measurementFromContext(ctx); m != nil {
		m.httpStatus = resp.StatusCode
		m.phase = "read"
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read LLM gateway async response: %w", err)
	}
	if m := measurementFromContext(ctx); m != nil {
		m.phase = "decode"
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			m.phase = "http"
		}
	}
	return resp, rawBody, nil
}

func decodeAsyncJob(rawBody []byte) (gatewayAsyncJob, error) {
	var job gatewayAsyncJob
	if err := json.Unmarshal(rawBody, &job); err != nil {
		return gatewayAsyncJob{}, fmt.Errorf("decode LLM gateway async response: %w", err)
	}
	job.ID = strings.TrimSpace(job.ID)
	job.Status = strings.TrimSpace(job.Status)
	return job, nil
}

func (c *Client) runAsync(ctx context.Context, submitPath string, payload []byte, sessionID string) (json.RawMessage, int, error) {
	submitEndpoint := c.BaseURL + submitPath
	resp, rawBody, err := c.doAsyncRequest(ctx, http.MethodPost, submitEndpoint, payload, sessionID)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusAccepted {
		return nil, 0, &asyncHTTPError{StatusCode: resp.StatusCode, Body: bodySnippet(rawBody)}
	}
	job, err := decodeAsyncJob(rawBody)
	if err != nil {
		return nil, 0, err
	}
	if job.ID == "" || (job.Status != "pending" && job.Status != "processing") {
		return nil, 0, fmt.Errorf("LLM gateway async submission returned invalid job state")
	}

	pollEndpoint := submitEndpoint + "/" + url.PathEscape(job.ID)
	pollFailures := 0
	for {
		if m := measurementFromContext(ctx); m != nil {
			m.phase = "poll"
		}
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, 0, ctx.Err()
		case <-timer.C:
		}

		pollResp, pollBody, pollErr := c.doAsyncRequest(ctx, http.MethodGet, pollEndpoint, nil, sessionID)
		if pollErr != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			pollFailures++
			if pollFailures >= maxAsyncPollFailures {
				return nil, 0, fmt.Errorf("poll LLM gateway async job: %w", pollErr)
			}
			continue
		}
		if pollResp.StatusCode >= http.StatusInternalServerError {
			pollFailures++
			if pollFailures >= maxAsyncPollFailures {
				return nil, 0, &asyncHTTPError{StatusCode: pollResp.StatusCode, Body: bodySnippet(pollBody)}
			}
			continue
		}
		pollFailures = 0
		if pollResp.StatusCode != http.StatusAccepted && pollResp.StatusCode != http.StatusOK {
			return nil, 0, &asyncHTTPError{StatusCode: pollResp.StatusCode, Body: bodySnippet(pollBody)}
		}
		polled, err := decodeAsyncJob(pollBody)
		if err != nil {
			return nil, 0, err
		}
		if polled.ID != job.ID {
			return nil, 0, fmt.Errorf("LLM gateway async poll returned mismatched job ID")
		}
		switch polled.Status {
		case "pending", "processing":
			if pollResp.StatusCode != http.StatusAccepted {
				return nil, 0, fmt.Errorf("LLM gateway async poll returned non-terminal status with HTTP %d", pollResp.StatusCode)
			}
			continue
		case "completed":
			if pollResp.StatusCode != http.StatusOK || len(polled.Result) == 0 || string(polled.Result) == "null" {
				return nil, 0, fmt.Errorf("LLM gateway async job completed without a result")
			}
			return polled.Result, polled.StatusCode, nil
		case "failed":
			if pollResp.StatusCode != http.StatusOK {
				return nil, 0, fmt.Errorf("LLM gateway async job failed with invalid HTTP status")
			}
			statusCode := polled.StatusCode
			if statusCode < 100 || statusCode > 599 {
				statusCode = http.StatusInternalServerError
			}
			return nil, 0, &asyncHTTPError{StatusCode: statusCode, Body: bodySnippet(polled.Error)}
		default:
			return nil, 0, fmt.Errorf("LLM gateway async poll returned unknown status")
		}
	}
}

// Embed submits text through sync /v1/embeddings first and falls back to
// Bifrost async polling when the sync endpoint is unavailable.
func (c *Client) Embed(ctx context.Context, req llm.EmbedRequest) (_ *llm.EmbedResponse, err error) {
	ctx, complete := c.beginMeasurement(ctx, req.Model, "embedding", "sync")
	defer func() { complete(err) }()
	payloadBytes, err := json.Marshal(gatewayEmbeddingRequest{Model: req.Model, Input: req.Input})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal embedding request: %w", err)
	}
	requestLog := c.requestLog(ctx, req.Model)
	rawBody, status, err := c.postSync(ctx, "/v1/embeddings", payloadBytes, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed && status != http.StatusNotImplemented && status != http.StatusAccepted {
			if m := measurementFromContext(ctx); m != nil {
				m.httpStatus = status
			}
			requestLog.Debug("provider.gateway.embed.http_error", "LLM gateway embedding HTTP failure", config.F("phase", "http"), config.F("http_status", status))
			return nil, &embedHTTPError{StatusCode: status}
		}
		rawBody, _, err = c.runAsync(ctx, "/v1/async/embeddings", payloadBytes, "")
		if err != nil {
			var httpErr *asyncHTTPError
			if errors.As(err, &httpErr) {
				measurementFromContext(ctx).httpStatus = httpErr.StatusCode
				requestLog.Debug("provider.gateway.embed.http_error", "LLM gateway embedding HTTP failure", config.F("phase", "http"), config.ErrorField(httpErr))
				return nil, &embedHTTPError{StatusCode: httpErr.StatusCode}
			}
			return nil, fmt.Errorf("LLM gateway async embed failed: %w", err)
		}
	}

	var gatewayResp gatewayEmbeddingResponse
	if err := json.Unmarshal(rawBody, &gatewayResp); err != nil {
		requestLog.Debug("provider.gateway.embed.decode_error", "failed to decode LLM gateway embed response", config.F("phase", "decode"), config.ErrorField(err))
		return nil, fmt.Errorf("failed to decode embedding response: %w", err)
	}
	measurementFromContext(ctx).usage = gatewayResp.Usage
	if gatewayResp.Error != nil {
		err := fmt.Errorf("LLM gateway embed response reported an error")
		requestLog.Debug("provider.gateway.embed.response_error", "LLM gateway embedding response failure", config.F("phase", "response"), config.ErrorField(err))
		return nil, err
	}
	if len(gatewayResp.Data) == 0 || len(gatewayResp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("LLM gateway embed response contained no embeddings")
	}

	embeddings := make([][]float64, 0, len(gatewayResp.Data))
	for _, datum := range gatewayResp.Data {
		embeddings = append(embeddings, datum.Embedding)
	}
	return &llm.EmbedResponse{Model: gatewayResp.Model, Embeddings: embeddings}, nil
}

func (c *Client) postSync(ctx context.Context, path string, payloadBytes []byte, sessionID string) (rawBody []byte, status int, err error) {
	endpoint := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to build embedding request: %w", err)
	}
	c.applyHeaders(req)
	applySessionHeader(req, sessionID)
	if m := measurementFromContext(ctx); m != nil {
		m.phase = "submit"
		m.submitted = ctx.Err() == nil && req.URL.Host != "" && (req.URL.Scheme == "http" || req.URL.Scheme == "https")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("LLM gateway embedding request failed: %w", err)
	}
	defer resp.Body.Close()
	if m := measurementFromContext(ctx); m != nil {
		m.httpStatus = resp.StatusCode
		m.phase = "read"
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read embedding response: %w", err)
	}
	if len(body) > 2<<20 {
		return nil, resp.StatusCode, fmt.Errorf("embedding response oversize")
	}
	return body, resp.StatusCode, nil
}

// Chat sends OpenAI-compatible chat requests. Streaming uses SSE; non-streaming
// tries sync POST /v1/chat/completions first and falls back to Bifrost async
// polling only when the sync endpoint is unavailable.
func (c *Client) Chat(ctx context.Context, req llm.ChatRequest, chatStreamCallback func(chunk llm.ChatMessage)) (_ *llm.ChatResponse, err error) {
	transport := "sync"
	if req.Stream {
		transport = "streaming"
	}
	ctx, complete := c.beginMeasurement(ctx, req.Model, "chat", transport)
	defer func() { complete(err) }()
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = sessionIDFromContext(ctx)
	}
	gatewayReq := gatewayChatRequest{Model: req.Model, User: req.User, Messages: mapToGatewayMessages(req.Messages), Tools: req.Tools, ToolChoice: req.ToolChoice, ParallelToolCalls: req.ParallelToolCalls, Temperature: req.Temperature, TopP: req.TopP, FrequencyPenalty: req.FrequencyPenalty, PresencePenalty: req.PresencePenalty, Seed: req.Seed, Stop: req.Stop, MaxTokens: req.MaxTokens, ResponseFormat: responseFormat(req.Format), Stream: req.Stream}
	if req.Stream {
		gatewayReq.StreamOptions = &gatewayStreamOptions{IncludeUsage: true}
	}

	startedAt := time.Now()
	if req.Stream {
		return c.doStreamChat(ctx, gatewayReq, req.Model, sessionID, startedAt, chatStreamCallback)
	}
	return c.doSyncChatWithFallback(ctx, gatewayReq, req.Model, sessionID, startedAt)
}

func (c *Client) doStreamChat(ctx context.Context, gatewayReq gatewayChatRequest, model, sessionID string, startedAt time.Time, chatStreamCallback func(chunk llm.ChatMessage)) (*llm.ChatResponse, error) {
	payloadBytes, err := json.Marshal(gatewayReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal chat request: %w", err)
	}
	measurementFromContext(ctx).phase = "submit"
	endpoint := fmt.Sprintf("%s/v1/chat/completions", c.BaseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create chat stream request: %w", err)
	}
	c.applyHeaders(httpReq)
	applySessionHeader(httpReq, sessionID)
	measurementFromContext(ctx).submitted = ctx.Err() == nil && httpReq.URL.Host != "" && (httpReq.URL.Scheme == "http" || httpReq.URL.Scheme == "https")
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("LLM gateway chat stream request failed: %w", err)
	}
	defer resp.Body.Close()
	if chatStreamCallback == nil {
		chatStreamCallback = func(llm.ChatMessage) {}
	}
	chatResp, streamErr := c.readChatStream(ctx, resp, model, startedAt, chatStreamCallback)
	if streamErr != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return chatResp, streamErr
}

// doSyncChatWithFallback tries sync chat, retries once on provider-specific
// field aliases, and falls back to async polling for Bifrost-style gateways.
func (c *Client) doSyncChatWithFallback(ctx context.Context, gatewayReq gatewayChatRequest, model, sessionID string, startedAt time.Time) (*llm.ChatResponse, error) {
	rawBody, status, bodySnippet, err := c.postSyncChat(ctx, gatewayReq, sessionID)
	if err != nil {
		return nil, err
	}
	if status == http.StatusOK {
		return c.decodeChatResponse(ctx, rawBody, model, startedAt)
	}
	// Alias retry: some providers only accept max_completion_tokens.
	if status == http.StatusBadRequest && gatewayReq.MaxTokens != nil && gatewayReq.MaxCompletionTokens == nil && containsFold(bodySnippet, "max_completion_tokens") {
		gatewayReq.MaxCompletionTokens = gatewayReq.MaxTokens
		gatewayReq.MaxTokens = nil
		rawBody, status, bodySnippet, err = c.postSyncChat(ctx, gatewayReq, sessionID)
		if err != nil {
			return nil, err
		}
		if status == http.StatusOK {
			return c.decodeChatResponse(ctx, rawBody, model, startedAt)
		}
	}
	// Alias retry: older servers reject parallel_tool_calls.
	if status == http.StatusBadRequest && gatewayReq.ParallelToolCalls != nil && containsFold(bodySnippet, "parallel_tool_calls") {
		gatewayReq.ParallelToolCalls = nil
		rawBody, status, bodySnippet, err = c.postSyncChat(ctx, gatewayReq, sessionID)
		if err != nil {
			return nil, err
		}
		if status == http.StatusOK {
			return c.decodeChatResponse(ctx, rawBody, model, startedAt)
		}
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented || status == http.StatusAccepted {
		payloadBytes, err := json.Marshal(gatewayReq)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal chat request: %w", err)
		}
		rawBody, _, err := c.runAsync(ctx, "/v1/async/chat/completions", payloadBytes, sessionID)
		if err != nil {
			var httpErr *asyncHTTPError
			if errors.As(err, &httpErr) {
				return nil, &llm.ChatHTTPError{StatusCode: httpErr.StatusCode, Body: httpErr.Body}
			}
			return nil, fmt.Errorf("LLM gateway async chat failed: %w", err)
		}
		return c.decodeChatResponse(ctx, rawBody, model, startedAt)
	}
	return nil, &llm.ChatHTTPError{StatusCode: status, Body: bodySnippet}
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func (c *Client) postSyncChat(ctx context.Context, gatewayReq gatewayChatRequest, sessionID string) (rawBody []byte, status int, snippet string, err error) {
	payloadBytes, err := json.Marshal(gatewayReq)
	if err != nil {
		return nil, 0, "", fmt.Errorf("failed to marshal chat request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/v1/chat/completions", c.BaseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, 0, "", fmt.Errorf("failed to create chat request: %w", err)
	}
	c.applyHeaders(httpReq)
	applySessionHeader(httpReq, sessionID)
	if m := measurementFromContext(ctx); m != nil {
		m.phase = "submit"
		m.submitted = ctx.Err() == nil && httpReq.URL.Host != "" && (httpReq.URL.Scheme == "http" || httpReq.URL.Scheme == "https")
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, "", ctx.Err()
		}
		return nil, 0, "", fmt.Errorf("LLM gateway chat request failed: %w", err)
	}
	defer resp.Body.Close()
	if m := measurementFromContext(ctx); m != nil {
		m.httpStatus = resp.StatusCode
		m.phase = "read"
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil {
		return nil, 0, "", fmt.Errorf("failed to read chat response: %w", err)
	}
	if len(body) > 2<<20 {
		return nil, resp.StatusCode, "", &llm.ChatHTTPError{StatusCode: resp.StatusCode, Body: "response oversize"}
	}
	return body, resp.StatusCode, bodySnippet(body), nil
}

func (c *Client) requestLog(ctx context.Context, model string) *config.Logger {
	return c.log.Server("provider.gateway", append(requestctx.LogFields(ctx), config.F("model", model))...)
}

func (c *Client) decodeChatResponse(ctx context.Context, rawBody []byte, model string, startedAt time.Time) (*llm.ChatResponse, error) {
	if m := measurementFromContext(ctx); m != nil {
		m.phase = "decode"
	}
	requestLog := c.requestLog(ctx, model)
	var gatewayResp gatewayChatResponse
	if err := json.Unmarshal(rawBody, &gatewayResp); err != nil {
		requestLog.Debug("provider.gateway.chat.decode_error", "failed to decode LLM gateway chat response", config.F("phase", "decode"), config.ErrorField(err))
		return nil, fmt.Errorf("failed to decode chat response: %w", err)
	}
	if m := measurementFromContext(ctx); m != nil {
		m.usage = gatewayResp.Usage
	}
	if gatewayResp.Error != nil {
		err := fmt.Errorf("LLM gateway chat response reported an error")
		requestLog.Debug("provider.gateway.chat.response_error", "LLM gateway chat response failure", config.F("phase", "response"), config.ErrorField(err))
		return nil, err
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
	doneReason := normalizeFinishReason(choice.FinishReason)
	if m := measurementFromContext(ctx); m != nil {
		m.doneReason = doneReason
	}

	return &llm.ChatResponse{
		Model:            firstNonEmpty(gatewayResp.Model, model),
		Message:          msg,
		PromptTokens:     gatewayResp.Usage.PromptTokens,
		CompletionTokens: gatewayResp.Usage.CompletionTokens,
		TotalTokens:      gatewayResp.Usage.TotalTokens,
		DurationMS:       durationMS,
		DoneReason:       doneReason,
	}, nil
}

func normalizeFinishReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case "function_call":
		return "tool_calls"
	default:
		return strings.TrimSpace(reason)
	}
}

func (c *Client) readChatStream(ctx context.Context, resp *http.Response, model string, startedAt time.Time, chatStreamCallback func(chunk llm.ChatMessage)) (*llm.ChatResponse, error) {
	requestLog := c.requestLog(ctx, model)
	m := measurementFromContext(ctx)
	if m == nil {
		m = &gatewayMeasurement{started: startedAt}
	}
	m.httpStatus = resp.StatusCode
	m.phase = "read"
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		m.phase = "http"
		rawBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("failed to read chat stream response body: %w", readErr)
		}
		snippet := bodySnippet(rawBody)
		err := &llm.ChatHTTPError{StatusCode: resp.StatusCode, Body: snippet}
		requestLog.Debug("provider.gateway.chat.http_error", "LLM gateway chat HTTP failure", config.F("phase", "http"), config.ErrorField(err))
		return nil, err
	}

	var final llm.ChatResponse
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
			m.malformed++
			if m.malformed == 1 && ctx.Err() == nil {
				requestLog.Warn("provider.gateway.chat.stream.parse_failed", "failed to parse LLM gateway chat stream chunk", config.F("phase", "decode"), config.F("status", "degraded"), config.ErrorField(err))
			}
			continue
		}
		if chunk.Usage.reported {
			m.observeUsage(chunk.Usage)
		}
		if chunk.Error != nil {
			err := fmt.Errorf("LLM gateway chat stream reported an error")
			requestLog.Debug("provider.gateway.chat.response_error", "LLM gateway chat stream response failure", config.F("phase", "response"), config.ErrorField(err))
			return nil, err
		}
		if chunk.Model != "" {
			final.Model = chunk.Model
		}
		if usageReported(chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens) {
			final.PromptTokens = chunk.Usage.PromptTokens
			final.CompletionTokens = chunk.Usage.CompletionTokens
			final.TotalTokens = chunk.Usage.TotalTokens
		}
		choice, ok := firstChoice(chunk)
		if !ok {
			continue
		}
		final.DoneReason = normalizeFinishReason(choice.FinishReason)
		if choice.FinishReason != "" {
			m.doneReason = normalizeFinishReason(choice.FinishReason)
		}
		content := contentToString(choice.Delta.Content)
		thinking := firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Thinking, choice.Delta.Reasoning)
		if m.firstOutput == nil && (thinking != "" || content != "") {
			elapsed := time.Since(m.started).Milliseconds()
			m.firstOutput = &elapsed
		}
		if thinking != "" {
			final.Message.Role = "assistant"
			final.Message.Thinking += thinking
			chatStreamCallback(llm.ChatMessage{Role: "assistant", Thinking: thinking})
		}
		if content != "" {
			final.Message.Role = "assistant"
			final.Message.Content += content
			chatStreamCallback(llm.ChatMessage{Role: "assistant", Content: content})
		}
		for _, tc := range choice.Delta.ToolCalls {
			idx := tc.Index
			part, ok := toolParts[idx]
			if !ok {
				part = &gatewayStreamToolCall{}
				toolParts[idx] = part
			} else if idx == 0 && tc.Function.Name != "" && part.Name != "" && tc.Function.Name != part.Name {
				// Some providers omit the index: disambiguate concurrent calls by name.
				idx = len(toolParts)
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
		requestLog.Debug("provider.gateway.chat.stream.scan_failed", "LLM gateway chat stream scan failed", config.F("phase", "read"), config.ErrorField(err))
		return nil, fmt.Errorf("read LLM gateway chat stream: %w", err)
	}

	if len(toolParts) > 0 {
		final.Message.Role = "assistant"
		indices := make([]int, 0, len(toolParts))
		for i := range toolParts {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		for _, i := range indices {
			part := toolParts[i]
			if part == nil || part.Name == "" {
				continue
			}
			final.Message.ToolCalls = append(final.Message.ToolCalls, llm.ToolCall{ID: firstNonEmpty(part.ID, fmt.Sprintf("call_stream_%d", i+1)), Function: llm.ToolFunction{Name: part.Name, Arguments: decodeToolArguments(part.Arguments), RawArguments: part.Arguments}})
		}
	}
	final.DurationMS = time.Since(startedAt).Milliseconds()
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
