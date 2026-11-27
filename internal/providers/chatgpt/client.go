package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// codexModelsClientVersion is sent as ?client_version= on the models listing.
// The Codex backend gates catalog visibility on the version format: older
// semvers return an empty catalog, so this pins a version known to list.
const codexModelsClientVersion = "1.0.0"

// maxCodexBodyBytes bounds one Codex response body or SSE payload read.
const maxCodexBodyBytes = 2 << 20

// Client authenticates with the ChatGPT subscription: it resolves the OAuth
// bearer per request (refreshing when expired) and speaks the Codex backend
// transport (chatgpt.com/backend-api/codex) natively — versioned model
// catalog plus streaming Responses. The refresh token is single-use; the
// store is the source of truth and concurrent refreshes coalesce.
type Client struct {
	baseURL    string
	virtualKey string
	httpClient *http.Client
	log        *config.Logger
	store      *Store
	provider   string
}

// NewClient binds a subscription client to one profile provider. A nil log
// is tolerated (debug paths discard); production passes the profile logger
// so gateway telemetry never nil-dereferences mid-turn.
func NewClient(baseURL, profileRoot, provider, virtualKey string, httpClient *http.Client, log *config.Logger) (*Client, error) {
	store, err := NewStore(profileRoot, provider)
	if err != nil {
		return nil, err
	}
	api := strings.TrimSpace(baseURL)
	if api == "" {
		api = DefaultAPI
	}
	return &Client{baseURL: api, virtualKey: strings.TrimSpace(virtualKey), httpClient: httpClient, log: log, store: store, provider: provider}, nil
}

// ensureToken loads the credential and refreshes it when expired.
func (c *Client) ensureToken(ctx context.Context) (TokenSet, error) {
	tokens, ok, err := c.store.Load(ctx)
	if err != nil {
		return TokenSet{}, err
	}
	if !ok {
		return TokenSet{}, errors.New("chatgpt subscription is not authorized")
	}
	if !Expired(tokens, time.Now()) {
		return tokens, nil
	}
	if strings.TrimSpace(tokens.RefreshToken) == "" {
		return tokens, nil
	}
	key := c.store.dir + "\x00" + c.provider
	refreshMu.Lock()
	if call, dup := refreshPromise[key]; dup {
		refreshMu.Unlock()
		select {
		case <-ctx.Done():
			return TokenSet{}, ctx.Err()
		case <-call.done:
			if call.err != nil {
				return TokenSet{}, call.err
			}
		}
		tokens, ok, err := c.store.Load(ctx)
		if err != nil {
			return TokenSet{}, err
		}
		if !ok {
			return TokenSet{}, errors.New("chatgpt subscription is not authorized")
		}
		return tokens, nil
	}
	call := &refreshCall{done: make(chan struct{})}
	refreshPromise[key] = call
	refreshMu.Unlock()
	call.err = func() error {
		current, ok, err := c.store.Load(ctx)
		if err != nil || !ok {
			return err
		}
		if !Expired(current, time.Now()) {
			return nil
		}
		resp, err := RefreshTokens(ctx, c.httpClient, current.RefreshToken)
		if err != nil {
			return err
		}
		next := TokenSet{AccessToken: resp.AccessToken, RefreshToken: resp.RefreshToken, AccountID: current.AccountID}
		if strings.TrimSpace(next.RefreshToken) == "" {
			next.RefreshToken = current.RefreshToken
		}
		if id := AccountIDFromIDToken(resp.IDToken); id != "" {
			next.AccountID = id
		}
		if resp.ExpiresIn > 0 {
			next.ExpiresAt = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).Unix()
		} else {
			next.ExpiresAt = current.ExpiresAt
		}
		return c.store.Save(ctx, next)
	}()
	close(call.done)
	refreshMu.Lock()
	delete(refreshPromise, key)
	refreshMu.Unlock()
	if call.err != nil {
		return TokenSet{}, call.err
	}
	tokens, ok, loadErr := c.store.Load(ctx)
	if loadErr != nil {
		return TokenSet{}, loadErr
	}
	if !ok {
		return TokenSet{}, errors.New("chatgpt subscription is not authorized")
	}
	return tokens, nil
}

// codexModelsResponse is the versioned catalog listing.
type codexModelsResponse struct {
	Models []struct {
		Slug          string `json:"slug"`
		ContextWindow int    `json:"context_window"`
		DisplayName   string `json:"display_name"`
		Default       bool   `json:"default"`
	} `json:"models"`
}

// ListModels fetches the subscription's Codex catalog. The endpoint requires
// a client version and returns only models the account may use, so unlike the
// gateway listing there is no static fallback: failures report an error and
// callers degrade to unavailable instead of advertising models that 404.
func (c *Client) ListModels(ctx context.Context) ([]llm.ModelEntry, error) {
	tokens, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(c.baseURL, "/") + "/models?client_version=" + codexModelsClientVersion
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("codex models request failed: %w", err)
	}
	c.applyAuth(req, tokens)
	resp, err := c.http().Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("codex models request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &llm.ChatHTTPError{StatusCode: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCodexBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("codex models request failed: %w", err)
	}
	if len(body) > maxCodexBodyBytes {
		return nil, fmt.Errorf("codex models response oversize")
	}
	var payload codexModelsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("codex models response malformed")
	}
	entries := make([]llm.ModelEntry, 0, len(payload.Models))
	for _, m := range payload.Models {
		id := strings.TrimSpace(m.Slug)
		if id == "" {
			continue
		}
		entry := llm.ModelEntry{ID: id}
		if m.ContextWindow > 0 {
			entry.Window = m.ContextWindow
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// codexContentPart is one message content part.
type codexContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// codexRequest is the Codex Responses body. The endpoint mandates streaming
// and forbids persistence; input must be a list.
type codexRequest struct {
	Model             string      `json:"model"`
	Instructions      string      `json:"instructions,omitempty"`
	Input             []any       `json:"input"`
	Tools             []codexTool `json:"tools,omitempty"`
	ToolChoice        any         `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool       `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64    `json:"temperature,omitempty"`
	TopP              *float64    `json:"top_p,omitempty"`
	MaxOutputTokens   *int        `json:"max_output_tokens,omitempty"`
	Stream            bool        `json:"stream"`
	Store             bool        `json:"store"`
}

// codexTool is one function tool in Codex shape.
type codexTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// codexMessageItem is a role/content input item.
type codexMessageItem struct {
	Role    string             `json:"role"`
	Content []codexContentPart `json:"content"`
}

// codexOutputItem is a function_call_output input item.
type codexOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// mapInput converts provider-neutral messages to Codex input plus separate
// system instructions. Tool-call history replays as function_call items so
// multi-turn tool loops keep their correlation.
func mapInput(messages []llm.ChatMessage) (string, []any) {
	var instructions []string
	input := make([]any, 0, len(messages))
	for _, m := range messages {
		role := strings.TrimSpace(m.Role)
		switch role {
		case "system", "developer":
			if text := strings.TrimSpace(m.Content); text != "" {
				instructions = append(instructions, text)
			}
			continue
		case "tool":
			callID := strings.TrimSpace(m.ToolCallID)
			if callID == "" {
				callID = strings.TrimSpace(m.ToolName)
			}
			if callID == "" {
				continue
			}
			input = append(input, codexOutputItem{Type: "function_call_output", CallID: callID, Output: m.Content})
			continue
		}
		if role == "" {
			role = "user"
		}
		parts := make([]codexContentPart, 0, 1+len(m.Images))
		// The Codex backend types message text by role: assistant history
		// replays as output_text, everything else as input_text. A wrong
		// type fails the whole turn with a 400.
		textType := "input_text"
		if role == "assistant" {
			textType = "output_text"
		}
		if strings.TrimSpace(m.Content) != "" {
			parts = append(parts, codexContentPart{Type: textType, Text: m.Content})
		}
		for _, image := range m.Images {
			mimeType := strings.TrimSpace(image.MimeType)
			if mimeType == "" {
				mimeType = "image/jpeg"
			}
			parts = append(parts, codexContentPart{Type: "input_image", ImageURL: "data:" + mimeType + ";base64," + image.Data})
		}
		if len(parts) == 0 && len(m.ToolCalls) == 0 {
			continue
		}
		item := codexMessageItem{Role: role, Content: parts}
		if len(m.ToolCalls) > 0 {
			if len(parts) > 0 {
				input = append(input, item)
			}
			for _, tc := range m.ToolCalls {
				raw := strings.TrimSpace(tc.Function.RawArguments)
				if raw == "" && len(tc.Function.Arguments) > 0 {
					if encoded, err := json.Marshal(tc.Function.Arguments); err == nil {
						raw = string(encoded)
					}
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   strings.TrimSpace(tc.ID),
					"name":      strings.TrimSpace(tc.Function.Name),
					"arguments": raw,
				})
			}
			continue
		}
		input = append(input, item)
	}
	return strings.Join(instructions, "\n\n"), input
}

// mapTools converts provider-neutral tools to Codex function tools.
func mapTools(tools []llm.Tool) []codexTool {
	out := make([]codexTool, 0, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			continue
		}
		tool := codexTool{Type: "function", Name: name, Description: strings.TrimSpace(t.Function.Description)}
		if t.Function.Parameters.Type != "" || len(t.Function.Parameters.Properties) > 0 {
			tool.Parameters = t.Function.Parameters
		}
		out = append(out, tool)
	}
	return out
}

// codexOutputContent is one message content part in a completed response.
type codexOutputContent struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Annotations []any  `json:"annotations,omitempty"`
}

// codexSummaryPart is one reasoning summary part.
type codexSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// codexOutput is one item of a completed response.
type codexOutput struct {
	Type      string               `json:"type"`
	ID        string               `json:"id,omitempty"`
	Content   []codexOutputContent `json:"content,omitempty"`
	Summary   []codexSummaryPart   `json:"summary,omitempty"`
	CallID    string               `json:"call_id,omitempty"`
	Name      string               `json:"name,omitempty"`
	Arguments string               `json:"arguments,omitempty"`
}

// codexErrorDetail is a structured backend failure.
type codexErrorDetail struct {
	Message string `json:"message,omitempty"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

// codexUsage mirrors the backend token accounting with presence tracking so
// absent usage is never mistaken for zero usage.
type codexUsage struct {
	reported, promptReported, completionReported, totalReported, reasoningReported bool
	PromptTokens, CompletionTokens, TotalTokens, ReasoningTokens                   int
}

func (u *codexUsage) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	at := func(key string) (int, bool) {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" {
			return 0, false
		}
		var v int
		if err := json.Unmarshal(raw, &v); err != nil || v < 0 {
			return 0, false
		}
		return v, true
	}
	if v, ok := at("input_tokens"); ok {
		u.PromptTokens, u.promptReported = v, true
	}
	if v, ok := at("output_tokens"); ok {
		u.CompletionTokens, u.completionReported = v, true
	}
	if v, ok := at("total_tokens"); ok {
		u.TotalTokens, u.totalReported = v, true
	}
	if raw, ok := fields["output_tokens_details"]; ok && string(raw) != "null" {
		var details struct {
			ReasoningTokens json.RawMessage `json:"reasoning_tokens"`
		}
		if err := json.Unmarshal(raw, &details); err == nil && len(details.ReasoningTokens) > 0 && string(details.ReasoningTokens) != "null" {
			var v int
			if err := json.Unmarshal(details.ReasoningTokens, &v); err == nil && v >= 0 {
				u.ReasoningTokens, u.reasoningReported = v, true
			}
		}
	}
	u.reported = u.promptReported || u.completionReported || u.totalReported || u.reasoningReported
	return nil
}

// codexCompleted is the terminal response object.
type codexCompleted struct {
	ID                string            `json:"id"`
	Model             string            `json:"model"`
	Status            string            `json:"status"`
	Output            []codexOutput     `json:"output"`
	Usage             codexUsage        `json:"usage"`
	Error             *codexErrorDetail `json:"error,omitempty"`
	IncompleteDetails *struct {
		Type   string `json:"type,omitempty"`
		Reason string `json:"reason,omitempty"`
	} `json:"incomplete_details,omitempty"`
}

// decodeCodexArguments parses a function_call arguments string without ever
// failing the turn: unparseable input rides as raw text.
func decodeCodexArguments(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return map[string]any{"_raw": raw}
	}
	return args
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// Chat implements llm.Chatter against the Codex Responses transport.
func (c *Client) Chat(ctx context.Context, req llm.ChatRequest, cb func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	tokens, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.doChat(ctx, tokens, req, cb)
	if err == nil {
		return resp, nil
	}
	if !isUnauthorized(err) || strings.TrimSpace(tokens.RefreshToken) == "" {
		return nil, err
	}
	if err := c.store.Clear(ctx); err != nil {
		return nil, err
	}
	refreshed, rerr := c.ensureToken(ctx)
	if rerr != nil {
		return nil, err
	}
	return c.doChat(ctx, refreshed, req, cb)
}

func (c *Client) doChat(ctx context.Context, tokens TokenSet, req llm.ChatRequest, cb func(llm.ChatMessage)) (_ *llm.ChatResponse, err error) {
	startedAt := time.Now()
	requestLog := c.log.Server("provider.gateway", append(requestctx.LogFields(ctx), config.F("model", req.Model))...)
	usage := requestctx.ModelUsage{Operation: "chat", Submitted: true}
	status, outcome := "ok", "ok"
	malformed := 0
	complete := func() {
		u := usage
		u.Status = status
		fields := []config.Field{
			config.F("operation", "chat"), config.F("transport", "streaming"),
			config.F("status", status), config.F("outcome", outcome),
			config.F("is_submitted", u.Submitted),
			config.F("is_usage_reported", u.UsageReported),
			config.F("is_usage_complete", u.UsageComplete),
			config.F("duration_ms", time.Since(startedAt).Milliseconds()),
		}
		if malformed > 0 {
			fields = append(fields, config.F("malformed_chunk_count", malformed))
		}
		if u.UsageReported {
			fields = append(fields, config.F("prompt_tokens", u.PromptTokens), config.F("completion_tokens", u.CompletionTokens), config.F("total_tokens", u.TotalTokens))
		}
		if err != nil {
			fields = append(fields, config.ErrorField(err))
		}
		requestLog.Debug("provider.gateway.chat.complete", "LLM gateway call completed", fields...)
		requestctx.UsageCollectorFromContext(ctx).Record(u)
	}
	defer func() { complete() }()

	instructions, input := mapInput(req.Messages)
	if len(input) == 0 {
		err = fmt.Errorf("codex chat requires at least one input message")
		status, outcome = "error", "error"
		return nil, err
	}
	body, err := json.Marshal(codexRequest{
		Model:             strings.TrimSpace(req.Model),
		Instructions:      instructions,
		Input:             input,
		Tools:             mapTools(req.Tools),
		ToolChoice:        codexToolChoice(req.ToolChoice),
		ParallelToolCalls: req.ParallelToolCalls,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		MaxOutputTokens:   req.MaxTokens,
		Stream:            true,
		Store:             false,
	})
	if err != nil {
		status, outcome = "error", "error"
		return nil, fmt.Errorf("codex chat request failed: %w", err)
	}
	endpoint := strings.TrimRight(c.baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		status, outcome = "error", "error"
		return nil, fmt.Errorf("codex chat request failed: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	c.applyAuth(httpReq, tokens)
	resp, err := c.http().Do(httpReq)
	if err != nil {
		status, outcome = "error", "error"
		return nil, fmt.Errorf("codex chat request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status, outcome = "error", "error"
		httpErr := &llm.ChatHTTPError{StatusCode: resp.StatusCode}
		requestLog.Debug("provider.gateway.chat.http_error", "LLM gateway chat HTTP failure", config.F("phase", "http"), config.ErrorField(httpErr))
		if _, code := decodeCodexFailure(io.LimitReader(resp.Body, 2048)); code == "model_not_found" {
			return nil, fmt.Errorf("codex model %q is not in the subscription catalog", strings.TrimSpace(req.Model))
		}
		return nil, httpErr
	}
	completed, content, thinking, calls, malformedChunks, ferr := c.readCodexStream(ctx, resp.Body, cb)
	malformed = malformedChunks
	if ferr != nil {
		status, outcome = "error", "error"
		return nil, ferr
	}
	final := &llm.ChatResponse{
		Model:            firstNonEmpty(completed.Model, strings.TrimSpace(req.Model)),
		Message:          llm.ChatMessage{Role: "assistant", Content: content, Thinking: thinking, ToolCalls: calls},
		DurationMS:       time.Since(startedAt).Milliseconds(),
		PromptTokens:     completed.Usage.PromptTokens,
		CompletionTokens: completed.Usage.CompletionTokens,
		TotalTokens:      completed.Usage.TotalTokens,
	}
	switch completed.Status {
	case "completed":
		final.DoneReason = "stop"
	case "incomplete":
		kind := ""
		if completed.IncompleteDetails != nil {
			kind = strings.TrimSpace(completed.IncompleteDetails.Type)
			if kind == "" {
				kind = strings.TrimSpace(completed.IncompleteDetails.Reason)
			}
		}
		switch kind {
		case "max_output_tokens", "length":
			final.DoneReason = "length"
		case "content_filter":
			final.DoneReason = "content_filter"
		default:
			final.DoneReason = "stop"
		}
	default:
		if completed.Error != nil && strings.TrimSpace(completed.Error.Message) != "" {
			status, outcome = "error", "error"
			return nil, fmt.Errorf("codex chat failed: %s", strings.TrimSpace(completed.Error.Message))
		}
		status, outcome = "error", "error"
		return nil, fmt.Errorf("codex chat failed (status %q)", strings.TrimSpace(completed.Status))
	}
	usage.Status = status
	usage.UsageReported = completed.Usage.reported
	usage.UsageComplete = completed.Usage.reported && completed.Usage.promptReported && completed.Usage.totalReported && completed.Usage.completionReported
	usage.PromptTokens = max(0, completed.Usage.PromptTokens)
	usage.CompletionTokens = max(0, completed.Usage.CompletionTokens)
	usage.TotalTokens = max(0, completed.Usage.TotalTokens)
	usage.ReasoningTokens = max(0, completed.Usage.ReasoningTokens)
	usage.DurationMS = final.DurationMS
	if malformed > 0 {
		status, outcome = "degraded", "degraded"
		usage.Status = status
	}
	return final, nil
}

// codexToolChoice maps the provider-neutral choice to the Codex shape.
// Only auto (omitted) and required ever arrive from the agent loop.
func codexToolChoice(choice llm.ToolChoice) any {
	switch strings.TrimSpace(string(choice)) {
	case "required":
		return "required"
	case "none":
		return "none"
	default:
		return nil
	}
}

// readCodexStream consumes the Codex SSE stream, forwarding content deltas
// and assembling the terminal completed object. Unknown events are ignored;
// malformed lines are counted so degraded streams stay visible in telemetry.
func (c *Client) readCodexStream(ctx context.Context, body io.Reader, cb func(llm.ChatMessage)) (completed codexCompleted, content, thinking string, calls []llm.ToolCall, malformed int, err error) {
	if cb == nil {
		cb = func(llm.ChatMessage) {}
	}
	var (
		contentBuf strings.Builder
		event      string
		data       []string
	)
	flush := func() {
		defer func() { event = ""; data = nil }()
		if len(data) == 0 {
			return
		}
		payload := strings.Join(data, "\n")
		if strings.TrimSpace(payload) == "[DONE]" {
			return
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			malformed++
			return
		}
		switch envelope.Type {
		case "response.output_text.delta":
			var chunk struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				malformed++
				return
			}
			if chunk.Delta != "" {
				contentBuf.WriteString(chunk.Delta)
				cb(llm.ChatMessage{Role: "assistant", Content: chunk.Delta})
			}
		case "response.completed":
			var done struct {
				Response codexCompleted `json:"response"`
			}
			if err := json.Unmarshal([]byte(payload), &done); err != nil {
				malformed++
				return
			}
			completed = done.Response
		case "response.failed":
			var failed struct {
				Response codexCompleted    `json:"response"`
				Error    *codexErrorDetail `json:"error"`
			}
			if err := json.Unmarshal([]byte(payload), &failed); err != nil {
				malformed++
				return
			}
			completed = failed.Response
			if completed.Status == "" {
				completed.Status = "failed"
			}
			if failed.Error != nil && completed.Error == nil {
				completed.Error = failed.Error
			}
		}
		_ = event
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return completed, "", "", nil, malformed, ctxErr
		}
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if rest, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(rest))
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		return completed, "", "", nil, malformed, fmt.Errorf("codex chat stream failed: %w", err)
	}
	flush()
	if completed.ID == "" && completed.Status == "" {
		return completed, "", "", nil, malformed, fmt.Errorf("codex chat stream ended without completion")
	}
	content, thinking, calls = assembleCodexOutput(completed.Output)
	return completed, content, thinking, calls, malformed, nil
}

// assembleCodexOutput builds the assistant message from a completed output
// list: message text, reasoning summaries, and function calls in order.
func assembleCodexOutput(output []codexOutput) (content, thinking string, calls []llm.ToolCall) {
	var contentBuf, thinkingBuf strings.Builder
	for i, item := range output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" && part.Text != "" {
					contentBuf.WriteString(part.Text)
				}
			}
		case "reasoning":
			for _, part := range item.Summary {
				if part.Type == "summary_text" && part.Text != "" {
					if thinkingBuf.Len() > 0 {
						thinkingBuf.WriteString("\n\n")
					}
					thinkingBuf.WriteString(part.Text)
				}
			}
		case "function_call":
			id := firstNonEmpty(item.CallID, item.ID, fmt.Sprintf("call_%d", i+1))
			name := strings.TrimSpace(item.Name)
			if name == "" {
				continue
			}
			calls = append(calls, llm.ToolCall{ID: id, Function: llm.ToolFunction{Name: name, Arguments: decodeCodexArguments(item.Arguments), RawArguments: strings.TrimSpace(item.Arguments)}})
		}
	}
	return contentBuf.String(), thinkingBuf.String(), calls
}

// decodeCodexFailure extracts a structured backend error without embedding
// response bodies: only the error code travels, never raw content.
func decodeCodexFailure(r io.Reader) (detail, code string) {
	body, err := io.ReadAll(io.LimitReader(r, 2048))
	if err != nil || len(body) == 0 {
		return "", ""
	}
	var payload struct {
		Detail string `json:"detail"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", ""
	}
	if payload.Error != nil {
		return strings.TrimSpace(payload.Detail), strings.TrimSpace(payload.Error.Code)
	}
	return strings.TrimSpace(payload.Detail), ""
}

// applyAuth attaches the subscription bearer and account headers.
func (c *Client) applyAuth(req *http.Request, tokens TokenSet) {
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tokens.AccessToken))
	if account := strings.TrimSpace(tokens.AccountID); account != "" {
		req.Header.Set("ChatGPT-Account-Id", account)
	}
	req.Header.Set("originator", "oswald")
}

func (c *Client) http() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return http.DefaultClient
}

func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *llm.ChatHTTPError
	switch {
	case errors.As(err, &httpErr):
		return httpErr.StatusCode == http.StatusUnauthorized
	default:
		return strings.Contains(strings.ToLower(err.Error()), "401")
	}
}
