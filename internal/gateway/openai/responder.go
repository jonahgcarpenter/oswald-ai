package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
)

type responseWriter struct {
	w            http.ResponseWriter
	ctx          context.Context
	id           string
	stream       bool
	includeUsage bool
	serviceTier  string
	sent         bool
	finished     bool
	created      int64
	streamErr    error
	// streamedContent tracks content text already flushed as live deltas so
	// the terminal response only sends the unstreamed remainder instead of
	// repeating the full answer. streamedTools counts oswald_tool_calls
	// entries already flushed live for the same dedup purpose.
	streamedContent string
	streamedTools   int
}

const (
	// oswaldToolStreamLimit bounds the details timeline per response so one
	// tool-heavy turn cannot balloon the SSE stream or buffered message.
	oswaldToolStreamLimit = 32
	// oswaldToolResultRunes bounds each server-side tool result in the API
	// timeline; durable history keeps the full text.
	oswaldToolResultRunes = 4000
	// oswaldToolArgsRunes bounds the encoded arguments object per entry.
	oswaldToolArgsRunes = 16000
)

func (r *responseWriter) StartProcessing() (func(), error) { return nil, nil }
func (r *responseWriter) SendFallback(string) error {
	return r.failure(http.StatusBadRequest, "invalid_request_error", "A user message is required.")
}
func (r *responseWriter) SendCommandResponse(commands.Result) error {
	return r.failure(http.StatusBadRequest, "invalid_request_error", "Commands are not supported.")
}
func (r *responseWriter) SendAgentError(string) error {
	return r.failure(http.StatusServiceUnavailable, "server_error", "Request could not be completed.")
}
func (r *responseWriter) CancelAgentResponse() error {
	return r.failure(http.StatusServiceUnavailable, "server_error", "Request was canceled.")
}

// Stream forwards live progress to SSE clients: reasoning tokens as
// reasoning_content, answer tokens as content, and server-side tool activity
// as the oswald_tool_calls extension. Caller-owned tool_calls keep the
// standard OpenAI field and are only sent with the terminal response.
// ChunkStatus (compaction notices) stays suppressed.
func (r *responseWriter) Stream(chunk agent.StreamChunk) {
	if !r.stream || r.finished || r.streamErr != nil || chunk.Type == "" {
		return
	}
	if !r.sent {
		if err := r.startStream(); err != nil {
			r.streamErr = err
			return
		}
	}
	switch chunk.Type {
	case agent.ChunkThinking:
		if chunk.Text != "" {
			r.streamErr = r.sendChoice(map[string]any{"reasoning_content": chunk.Text}, nil)
		}
	case agent.ChunkContent:
		if chunk.Text != "" {
			r.streamedContent += chunk.Text
			r.streamErr = r.sendChoice(map[string]any{"content": chunk.Text}, nil)
		}
	case agent.ChunkToolCall:
		if chunk.Tool == nil || chunk.Tool.Name == "" || r.streamedTools >= oswaldToolStreamLimit {
			return
		}
		event := agent.OswaldToolEvent{Name: chunk.Tool.Name, Phase: "call", Arguments: chunk.Tool.Arguments}
		r.streamedTools++
		r.streamErr = r.sendChoice(map[string]any{"oswald_tool_calls": []any{wireOswaldToolEvent(event)}}, nil)
	case agent.ChunkToolResult:
		if chunk.Tool == nil || chunk.Tool.Name == "" || r.streamedTools >= oswaldToolStreamLimit {
			return
		}
		event := agent.OswaldToolEvent{Name: chunk.Tool.Name, Phase: "result", ResultText: chunk.Tool.ResultText, IsError: chunk.Tool.IsError, DurationMS: chunk.Tool.DurationMS, WebSearch: chunk.Tool.WebSearch}
		r.streamedTools++
		r.streamErr = r.sendChoice(map[string]any{"oswald_tool_calls": []any{wireOswaldToolEvent(event)}}, nil)
	}
}

func (r *responseWriter) startStream() error {
	if err := r.contextErr(); err != nil {
		return err
	}
	r.w.Header().Set("Content-Type", "text/event-stream")
	r.w.Header().Set("Cache-Control", "no-cache")
	r.created = time.Now().Unix()
	return r.sendChoice(map[string]any{"role": "assistant"}, nil)
}

func (r *responseWriter) sendChoice(delta map[string]any, finish any) error {
	if err := r.contextErr(); err != nil {
		return err
	}
	chunk := map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created, "model": "oswald", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	if r.serviceTier != "" {
		chunk["service_tier"] = r.serviceTier
	}
	payload, _ := json.Marshal(chunk)
	r.sent = true
	if err := r.writeFrame(fmt.Sprintf("data: %s\n\n", payload)); err != nil {
		return err
	}
	return r.flush()
}

func (r *responseWriter) flush() error {
	if err := http.NewResponseController(r.w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return r.contextErr()
}

// truncateWireRunes caps UI timeline text without breaking UTF-8.
func truncateWireRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit])
}

// wireOswaldToolEvent renders one server-side tool timeline entry. Arguments
// are passed through as a JSON-safe object (already redacted upstream: no
// image bytes or reasoning); result text is rune-bounded for the stream.
func wireOswaldToolEvent(event agent.OswaldToolEvent) map[string]any {
	entry := map[string]any{"name": event.Name, "phase": event.Phase}
	switch event.Phase {
	case "call":
		if len(event.Arguments) > 0 {
			args := map[string]any{}
			for key, value := range event.Arguments {
				args[key] = value
			}
			if encoded, err := json.Marshal(args); err == nil && utf8.RuneCountInString(string(encoded)) > oswaldToolArgsRunes {
				entry["arguments_truncated"] = true
			}
			entry["arguments"] = args
		}
	case "result":
		if event.ResultText != "" {
			text := truncateWireRunes(event.ResultText, oswaldToolResultRunes)
			entry["result_text"] = text
			if text != event.ResultText {
				entry["result_truncated"] = true
			}
		}
		if event.IsError {
			entry["is_error"] = true
		}
		if event.DurationMS > 0 {
			entry["duration_ms"] = event.DurationMS
		}
		if event.WebSearch != nil {
			entry["web_search"] = event.WebSearch
		}
	default:
		entry["phase"] = "result"
		if event.ResultText != "" {
			entry["result_text"] = truncateWireRunes(event.ResultText, oswaldToolResultRunes)
		}
	}
	return entry
}

// wireOswaldTools renders the server-side details timeline, bounded so one
// turn cannot balloon the response. Caller-owned tool_calls are unaffected.
func wireOswaldTools(events []agent.OswaldToolEvent) []any {
	if len(events) == 0 {
		return nil
	}
	bounded := events
	if len(bounded) > oswaldToolStreamLimit {
		bounded = bounded[:oswaldToolStreamLimit]
	}
	out := make([]any, 0, len(bounded))
	for _, event := range bounded {
		if event.Name == "" {
			continue
		}
		phase := event.Phase
		if phase != "call" && phase != "result" {
			continue
		}
		out = append(out, wireOswaldToolEvent(event))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unstreamedOswaldTools returns timeline entries not yet flushed live.
func (r *responseWriter) unstreamedOswaldTools(events []agent.OswaldToolEvent) []agent.OswaldToolEvent {
	if r.streamedTools < 0 {
		r.streamedTools = 0
	}
	if r.streamedTools >= len(events) {
		return nil
	}
	remaining := events[r.streamedTools:]
	if len(remaining) > oswaldToolStreamLimit-r.streamedTools {
		remaining = remaining[:oswaldToolStreamLimit-r.streamedTools]
	}
	return remaining
}

// sendOswaldTimeline flushes unstreamed server-side tool entries as live
// deltas. In live requests most entries already streamed via Stream().
func (r *responseWriter) sendOswaldTimeline(events []agent.OswaldToolEvent) error {
	for _, event := range r.unstreamedOswaldTools(events) {
		if r.streamedTools >= oswaldToolStreamLimit {
			break
		}
		r.streamedTools++
		if err := r.sendChoice(map[string]any{"oswald_tool_calls": []any{wireOswaldToolEvent(event)}}, nil); err != nil {
			return err
		}
	}
	return nil
}

// unstreamedContent returns the terminal answer suffix not yet flushed live.
// Intermediate drafts before tool calls may already be streamed; when the
// final answer no longer starts with them the full text is sent so nothing
// is lost.
func (r *responseWriter) unstreamedContent(final string) string {
	if r.streamedContent == "" || final == "" {
		return final
	}
	if strings.HasPrefix(final, r.streamedContent) {
		return final[len(r.streamedContent):]
	}
	return final
}

func wireToolCalls(calls []struct {
	ID        string
	Name      string
	Arguments string
}) []any {
	out := make([]any, 0, len(calls))
	for _, call := range calls {
		out = append(out, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
	}
	return out
}

// finishReason maps the agent's terminal stop reason to the OpenAI
// finish_reason. Only the spec reasons this gateway can produce are
// reported; anything else falls back to "stop" without leaking provider
// internals. Tool-call turns report "tool_calls" separately.
func finishReason(response *agent.Response) string {
	if response != nil && (response.FinishReason == "length" || response.FinishReason == "content_filter") {
		return response.FinishReason
	}
	return "stop"
}

// wireUsage returns observed token totals, or nil when no provider call
// reported usage. Gateways must omit usage rather than invent it.
func wireUsage(response *agent.Response) map[string]any {
	if response == nil || !response.UsageReported {
		return nil
	}
	return map[string]any{"prompt_tokens": response.UsagePromptTokens, "completion_tokens": response.UsageCompletionTokens, "total_tokens": response.UsageTotalTokens}
}

func (r *responseWriter) SendAgentResponse(response *agent.Response) error {
	if err := r.contextErr(); err != nil {
		return err
	}
	if r.streamErr != nil {
		return r.streamErr
	}
	if response == nil || response.Error != "" || response.Kind == "provider_error" {
		return r.SendAgentError("")
	}
	if response.ServiceTier != "" {
		r.serviceTier = response.ServiceTier
	}
	if response.Kind == "over_budget" {
		return r.failure(http.StatusBadRequest, "invalid_request_error", "Message history exceeds the model context budget.")
	}
	if response.Kind == "invalid_tool_choice" {
		if response.ToolChoiceError == "no_tools_available" {
			return r.failure(http.StatusBadRequest, "invalid_request_error", "Tool choice requires available tools.")
		}
		return r.failure(http.StatusBadRequest, "invalid_request_error", "Named tool is not provided.")
	}
	if len(response.Attachments) > 0 {
		return r.failure(http.StatusUnprocessableEntity, "unsupported_response", "Image attachments are not supported.")
	}
	if r.finished || (!r.stream && r.sent) {
		return errors.New("response already sent")
	}
	if len(response.ToolCalls) > 0 {
		return r.sendToolCalls(response)
	}
	if r.stream {
		if !r.sent {
			if err := r.startStream(); err != nil {
				return err
			}
		}
		if err := r.sendOswaldTimeline(response.OswaldTools); err != nil {
			return err
		}
		if remainder := r.unstreamedContent(response.Response); remainder != "" {
			if err := r.sendChoice(map[string]any{"content": remainder}, nil); err != nil {
				return err
			}
		} else if r.streamedTools == 0 && len(response.OswaldTools) == 0 && r.streamedContent == "" && response.Response == "" {
			if err := r.sendChoice(map[string]any{"content": ""}, nil); err != nil {
				return err
			}
		}
		if err := r.sendChoice(map[string]any{}, finishReason(response)); err != nil {
			return err
		}
		r.finished = true
		if r.includeUsage {
			if usage := wireUsage(response); usage != nil {
				payload, _ := json.Marshal(map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created, "model": "oswald", "choices": []any{}, "usage": usage})
				r.sent = true
				if err := r.writeFrame(fmt.Sprintf("data: %s\n\n", payload)); err != nil {
					return err
				}
			}
		}
		if err := r.writeFrame("data: [DONE]\n\n"); err != nil {
			return err
		}
		return r.flush()
	}
	message := map[string]any{"role": "assistant", "content": response.Response}
	if response.Thinking != "" {
		message["reasoning_content"] = response.Thinking
	}
	if timeline := wireOswaldTools(response.OswaldTools); len(timeline) > 0 {
		message["oswald_tool_calls"] = timeline
	}
	// Stateless agent requests may make several provider calls; usage carries
	// observed request totals and is omitted when nothing reported.
	completion := map[string]any{"id": r.id, "object": "chat.completion", "created": time.Now().Unix(), "model": "oswald", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason(response)}}}
	if r.serviceTier != "" {
		completion["service_tier"] = r.serviceTier
	}
	if usage := wireUsage(response); usage != nil {
		completion["usage"] = usage
	}
	return r.sendJSON(http.StatusOK, completion)
}

func (r *responseWriter) sendToolCalls(response *agent.Response) error {
	calls := make([]struct {
		ID        string
		Name      string
		Arguments string
	}, 0, len(response.ToolCalls))
	for _, call := range response.ToolCalls {
		args := call.Function.RawArguments
		if args == "" {
			encoded, _ := json.Marshal(call.Function.Arguments)
			if len(encoded) > 0 {
				args = string(encoded)
			} else {
				args = "{}"
			}
		}
		calls = append(calls, struct {
			ID        string
			Name      string
			Arguments string
		}{ID: call.ID, Name: call.Function.Name, Arguments: args})
	}
	if r.stream {
		if !r.sent {
			if err := r.startStream(); err != nil {
				return err
			}
		}
		if err := r.sendOswaldTimeline(response.OswaldTools); err != nil {
			return err
		}
		for i, call := range calls {
			delta := map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}}}}
			if err := r.sendChoice(delta, nil); err != nil {
				return err
			}
		}
		if err := r.sendChoice(map[string]any{}, "tool_calls"); err != nil {
			return err
		}
		r.finished = true
		if r.includeUsage {
			if usage := wireUsage(response); usage != nil {
				payload, _ := json.Marshal(map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created, "model": "oswald", "choices": []any{}, "usage": usage})
				r.sent = true
				if err := r.writeFrame(fmt.Sprintf("data: %s\n\n", payload)); err != nil {
					return err
				}
			}
		}
		if err := r.writeFrame("data: [DONE]\n\n"); err != nil {
			return err
		}
		return r.flush()
	}
	var content any
	if response.Response != "" {
		content = response.Response
	}
	message := map[string]any{"role": "assistant", "content": content, "tool_calls": wireToolCalls(calls)}
	if response.Thinking != "" {
		message["reasoning_content"] = response.Thinking
	}
	if timeline := wireOswaldTools(response.OswaldTools); len(timeline) > 0 {
		message["oswald_tool_calls"] = timeline
	}
	completion := map[string]any{"id": r.id, "object": "chat.completion", "created": time.Now().Unix(), "model": "oswald", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "tool_calls"}}}
	if r.serviceTier != "" {
		completion["service_tier"] = r.serviceTier
	}
	if usage := wireUsage(response); usage != nil {
		completion["usage"] = usage
	}
	return r.sendJSON(http.StatusOK, completion)
}

func (r *responseWriter) writeFrame(frame string) error {
	n, err := io.WriteString(r.w, frame)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}

func (r *responseWriter) failure(code int, kind, message string) error {
	if err := r.contextErr(); err != nil {
		return err
	}
	if r.streamErr != nil {
		return r.streamErr
	}
	if r.finished || (!r.stream && r.sent) {
		return errors.New("response already sent")
	}
	if r.stream && r.sent {
		r.finished = true
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": kind}})
		if err := r.writeFrame(fmt.Sprintf("data: %s\n\n", payload)); err != nil {
			return err
		}
		return r.flush()
	}
	return r.sendJSON(code, map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": kind}})
}

func (r *responseWriter) contextErr() error {
	if r.ctx != nil {
		return r.ctx.Err()
	}
	return nil
}

func (r *responseWriter) sendJSON(code int, value any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(value); err != nil {
		return err
	}
	if err := r.contextErr(); err != nil {
		return err
	}
	r.w.Header().Set("Content-Type", "application/json")
	r.sent = true
	r.w.WriteHeader(code)
	n, err := r.w.Write(buf.Bytes())
	if err == nil && n != buf.Len() {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = r.contextErr()
	}
	return err
}
