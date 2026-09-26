// Package openai exposes a bounded, text-only OpenAI-compatible chat gateway.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

const maxBody = 256 << 10

// Gateway serves the local OpenAI-compatible API.
type Gateway struct {
	port     string
	accounts *accounts.Service
	deps     gatewayruntime.Dependencies
	model    string
	log      *config.Logger
	broker   *broker.Broker
	// Tests replace these seams without constructing a broker or account database.
	authenticate func(context.Context, string) (identity.Principal, error)
	execute      func(gatewayruntime.Request, gatewayruntime.Dependencies, gatewayruntime.Responder) gatewayruntime.Outcome
}

// New validates and constructs an OpenAI-compatible gateway.
func New(port string, accounts *accounts.Service, deps gatewayruntime.Dependencies, model string, log *config.Logger) (*Gateway, error) {
	n, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("openai port must be an integer from 1 through 65535")
	}
	if accounts == nil || log == nil || model == "" {
		return nil, fmt.Errorf("openai gateway dependencies are incomplete")
	}
	return &Gateway{port: strconv.Itoa(n), accounts: accounts, deps: deps, model: model, log: log, execute: gatewayruntime.Execute}, nil
}

// Name identifies this gateway.
func (g *Gateway) Name() string { return "openai" }

// Start serves the loopback HTTP API until the listener terminates.
func (g *Gateway) Start(b *broker.Broker) error {
	g.broker = b
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", g.port))
	if err != nil {
		return err
	}
	g.log.Server("gateway.openai").Info("gateway.listen", "openai gateway listening", config.F("port", g.port))
	return http.Serve(listener, g.Handler())
}

// Handler returns the HTTP API handler for mounting or local testing.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", g.models)
	mux.HandleFunc("/v1/chat/completions", g.completions)
	return mux
}

func (g *Gateway) authorize(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	start := time.Now()
	status, reason := "rejected", "invalid_header"
	defer func() {
		fields := []config.Field{config.F("record_kind", "measurement"), config.F("gateway", "openai"), config.F("duration_ms", time.Since(start).Milliseconds()), config.F("status", status), config.F("reason_code", reason)}
		log := g.log.Server("gateway.openai")
		if status == "error" {
			log.Warn("gateway.openai.auth.complete", "completed openai authentication", fields...)
		} else {
			log.Info("gateway.openai.auth.complete", "completed openai authentication", fields...)
		}
	}()
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "A single bearer token is required.")
		return identity.Principal{}, false
	}
	parts := strings.Split(headers[0], " ")
	if len(parts) != 2 || parts[0] != "Bearer" || parts[1] == "" || strings.TrimSpace(parts[1]) != parts[1] || strings.ContainsAny(parts[1], "\t,\r\n") {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "A single bearer token is required.")
		return identity.Principal{}, false
	}
	auth := g.authenticate
	if auth == nil {
		auth = g.accounts.AuthenticateAPIKey
	}
	principal, err := auth(r.Context(), parts[1])
	if err != nil && !errors.Is(err, accounts.ErrInvalidAPIKey) {
		status, reason = "error", "auth_unavailable"
		writeError(w, http.StatusServiceUnavailable, "server_error", "Authentication is temporarily unavailable.")
		return identity.Principal{}, false
	}
	if err != nil || !principal.Authenticated() || principal.Gateway != "openai" {
		reason = "invalid_key"
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key.")
		return identity.Principal{}, false
	}
	status, reason = "ok", "authenticated"
	return principal, true
}

func (g *Gateway) reject(w http.ResponseWriter, code int, reason, message string) {
	g.log.Server("gateway.openai").Info("gateway.openai.request.rejected", "rejected openai request before runtime", config.F("record_kind", "measurement"), config.F("gateway", "openai"), config.F("status", "rejected"), config.F("reason_code", reason))
	writeError(w, code, "invalid_request_error", message)
}

func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		g.reject(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}
	if _, ok := g.authorize(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{map[string]any{"id": "oswald", "object": "model", "created": 0, "owned_by": "oswald"}}})
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

func (g *Gateway) completions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		g.reject(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}
	principal, ok := g.authorize(w, r)
	if !ok {
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		g.reject(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input chatRequest
	if err := decoder.Decode(&input); err != nil {
		g.reject(w, http.StatusBadRequest, "invalid_json", "Invalid or unsupported JSON request.")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		g.reject(w, http.StatusBadRequest, "invalid_json", "Invalid or oversized JSON request.")
		return
	}
	if input.Model != "oswald" {
		g.reject(w, http.StatusBadRequest, "unsupported_model", "Only model oswald is supported.")
		return
	}
	if len(input.Messages) == 0 || len(input.Messages) > 32 {
		g.reject(w, http.StatusBadRequest, "invalid_messages", "Messages must contain 1 to 32 turns.")
		return
	}
	history := make([]llm.ChatMessage, 0, len(input.Messages)-1)
	total := 0
	for i, msg := range input.Messages {
		if msg.Role != "user" && msg.Role != "assistant" || (i == len(input.Messages)-1 && msg.Role != "user") {
			g.reject(w, http.StatusBadRequest, "unsupported_role", "Only user and assistant messages ending in a user message are supported.")
			return
		}
		var text string
		if len(msg.Content) == 0 || bytes.Equal(msg.Content, []byte("null")) || json.Unmarshal(msg.Content, &text) != nil {
			g.reject(w, http.StatusBadRequest, "unsupported_content", "Only text message content is supported; images are not supported.")
			return
		}
		total += len(text)
		if total > 128<<10 || len(text) > 32<<10 {
			g.reject(w, http.StatusBadRequest, "history_limit", "Message history exceeds limits.")
			return
		}
		if i == len(input.Messages)-1 {
			if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), "/") {
				g.reject(w, http.StatusBadRequest, "invalid_prompt", "A nonempty, non-command user message is required.")
				return
			}
			deps := g.deps
			if g.broker != nil {
				deps.Broker = g.broker
			}
			if deps.Access == nil {
				deps.Access = g.accounts
			}
			if deps.Log == nil {
				deps.Log = g.log
			}
			id := config.NewRequestID()
			responder := &responseWriter{w: w, ctx: r.Context(), id: "chatcmpl-" + id, stream: input.Stream}
			outcome := g.execute(gatewayruntime.Request{ReceivedAt: time.Now(), RequestID: id, ChatID: "openai:" + id, SessionKey: "openai:" + id, Principal: principal, IsDirect: true, IsMention: true, Text: text, PublicUserText: text, ClientHistory: history, Stateless: true}, deps, responder)
			if !responder.sent && r.Context().Err() == nil {
				if outcome.Action == routing.ActionIgnore && outcome.Reason == "user_banned" {
					w.WriteHeader(http.StatusNoContent)
				} else {
					writeError(w, http.StatusServiceUnavailable, "server_error", "Request could not be completed.")
				}
			}
			return
		}
		history = append(history, llm.ChatMessage{Role: msg.Role, Content: text})
	}
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, code int, kind, message string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": message, "type": kind, "code": kind}})
}
