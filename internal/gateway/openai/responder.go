package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
)

type responseWriter struct {
	w         http.ResponseWriter
	ctx       context.Context
	id        string
	stream    bool
	sent      bool
	finished  bool
	created   int64
	streamErr error
}

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

// Stream forwards model reasoning without publishing provisional answer text
// or Oswald's internal tool arguments and results.
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
	if chunk.Type == agent.ChunkThinking && chunk.Text != "" {
		r.streamErr = r.sendChoice(map[string]any{"reasoning_content": chunk.Text}, nil)
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
	payload, _ := json.Marshal(map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created, "model": "oswald", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
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
	if len(response.Attachments) > 0 {
		return r.failure(http.StatusUnprocessableEntity, "unsupported_response", "Image attachments are not supported.")
	}
	if r.finished || (!r.stream && r.sent) {
		return errors.New("response already sent")
	}
	if r.stream {
		if !r.sent {
			if err := r.startStream(); err != nil {
				return err
			}
		}
		if err := r.sendChoice(map[string]any{"content": response.Response}, nil); err != nil {
			return err
		}
		if err := r.sendChoice(map[string]any{}, "stop"); err != nil {
			return err
		}
		r.finished = true
		if err := r.writeFrame("data: [DONE]\n\n"); err != nil {
			return err
		}
		return r.flush()
	}
	// Stateless agent requests may make several provider calls; response.Metrics
	// describes only the last one and cannot represent completion usage.
	return r.sendJSON(http.StatusOK, map[string]any{"id": r.id, "object": "chat.completion", "created": time.Now().Unix(), "model": "oswald", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": response.Response}, "finish_reason": "stop"}}})
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
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": kind, "code": kind}})
		if err := r.writeFrame(fmt.Sprintf("data: %s\n\n", payload)); err != nil {
			return err
		}
		return r.flush()
	}
	return r.sendJSON(code, map[string]any{"error": map[string]any{"message": message, "type": kind, "code": kind}})
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
