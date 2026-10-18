// Package openai exposes a stateless OpenAI-compatible chat gateway.
package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway/routing"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

const maxBody = 2 << 20

var clientToolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// clientParticipantNameRE bounds the OpenAI participant name field.
var clientParticipantNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// parseParticipantName validates the optional message name field. Tool
// messages never take a name; other roles accept the spec charset.
func parseParticipantName(msg chatMessage) (string, bool) {
	if msg.Name == nil {
		return "", true
	}
	name := strings.TrimSpace(*msg.Name)
	if msg.Role == "tool" || !clientParticipantNameRE.MatchString(name) {
		return "", false
	}
	return name, true
}

// Gateway serves the stateless OpenAI-compatible API.
type Gateway struct {
	host     string
	port     string
	accounts identity.Resolver
	deps     gatewayruntime.Dependencies
	model    string
	log      *config.Logger
	broker   *broker.Broker
	// Tests replace these seams without constructing a broker or keyring.
	resolveKey func(string) (identity.Principal, error)
	execute    func(gatewayruntime.Request, gatewayruntime.Dependencies, gatewayruntime.Responder) gatewayruntime.Outcome
}

// New validates and constructs an OpenAI-compatible gateway.
func New(host, port string, accounts identity.Resolver, deps gatewayruntime.Dependencies, model string, log *config.Logger) (*Gateway, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		host = "127.0.0.1"
	}
	if host != "127.0.0.1" && host != "0.0.0.0" {
		return nil, fmt.Errorf("openai host must be 127.0.0.1 or 0.0.0.0")
	}
	n, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("openai port must be an integer from 1 through 65535")
	}
	if accounts == nil || log == nil || model == "" {
		return nil, fmt.Errorf("openai gateway dependencies are incomplete")
	}
	return &Gateway{host: host, port: strconv.Itoa(n), accounts: accounts, deps: deps, model: model, log: log, execute: gatewayruntime.Execute}, nil
}

// Name identifies this gateway.
func (g *Gateway) Name() string { return "openai" }

// Start serves the OpenAI HTTP API until the listener terminates.
func (g *Gateway) Start(b *broker.Broker) error {
	g.broker = b
	listener, err := net.Listen("tcp", net.JoinHostPort(g.host, g.port))
	if err != nil {
		return err
	}
	g.log.Server("gateway.openai").Debug("gateway.listen", "openai gateway listener starting", config.F("port", g.port))
	g.logConnected(listener.Addr())
	return http.Serve(listener, g.Handler())
}

// logConnected reports listener readiness on the INFO lifecycle stream. It
// runs only after the HTTP listener binds and carries no credential or
// request material.
func (g *Gateway) logConnected(addr net.Addr) {
	listenAddr := ""
	if addr != nil {
		listenAddr = addr.String()
		if host, _, splitErr := net.SplitHostPort(listenAddr); splitErr == nil {
			listenAddr = host
		}
	}
	g.log.Server("gateway.openai").Info("gateway.connected", "openai gateway ready",
		config.F("gateway", "openai"),
		config.F("listen_addr", listenAddr),
		config.F("port", g.port))
}

// Handler returns the HTTP API handler for mounting or local testing.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", g.models)
	mux.HandleFunc("/v1/models/", g.retrieveModel)
	mux.HandleFunc("/v1/chat/completions", g.completions)
	return mux
}

func bearerKey(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found {
		return ""
	}
	if !strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func (g *Gateway) resolveKeyPrincipal(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	start := time.Now()
	status, reason := "error", "invalid_api_key"
	var profile string
	defer func() {
		fields := []config.Field{config.F("gateway", "openai"), config.F("duration_ms", time.Since(start).Milliseconds()), config.F("status", status), config.F("reason_code", reason)}
		if profile != "" {
			fields = append(fields, config.F("profile", profile))
		}
		log := g.log.Server("gateway.openai")
		if status == "error" {
			log.Warn("gateway.openai.identity.complete", "resolved openai identity", fields...)
		} else {
			log.Debug("gateway.openai.identity.complete", "resolved openai identity", fields...)
		}
	}()
	raw := bearerKey(r)
	if raw == "" || len(raw) > 4096 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "Invalid API key.")
		return identity.Principal{}, false
	}
	resolve := g.resolveKey
	if resolve == nil {
		resolve = g.accounts.ResolveAPIKey
	}
	principal, err := resolve(raw)
	if err != nil || !principal.Authenticated() || principal.Gateway != "openai" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "Invalid API key.")
		return identity.Principal{}, false
	}
	status, reason, profile = "ok", "api_key", principal.CanonicalUserID
	return principal, true
}

func (g *Gateway) reject(w http.ResponseWriter, code int, reason, message string) {
	g.log.Server("gateway.openai").Debug("gateway.openai.request.rejected", "rejected openai request before runtime", config.F("gateway", "openai"), config.F("status", "rejected"), config.F("reason_code", reason))
	writeError(w, code, "invalid_request_error", message)
}

func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		g.reject(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}
	if _, ok := g.resolveKeyPrincipal(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{oswaldModel()}})
}

// retrieveModel serves the OpenAI retrieve-model endpoint for the single
// supported model id. Unknown ids fail as JSON errors, never the mux's
// HTML page.
func (g *Gateway) retrieveModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		g.reject(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}
	if _, ok := g.resolveKeyPrincipal(w, r); !ok {
		return
	}
	if strings.TrimPrefix(r.URL.Path, "/v1/models/") != "oswald" {
		g.reject(w, http.StatusNotFound, "not_found", "No such model.")
		return
	}
	writeJSON(w, http.StatusOK, oswaldModel())
}

// oswaldModel returns the spec-pure model object: no additive discovery
// fields. Clients declare their own capabilities and limits locally.
func oswaldModel() map[string]any {
	return map[string]any{"id": "oswald", "object": "model", "created": 0, "owned_by": "oswald"}
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             *string         `json:"name"`
	ReasoningContent *string         `json:"reasoning_content"`
	ToolCalls        []wireToolCall  `json:"tool_calls"`
	ToolCallID       *string         `json:"tool_call_id"`
}

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens           *int              `json:"max_tokens"`
	MaxCompletionTokens *int              `json:"max_completion_tokens"`
	Tools               []json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls"`
	Temperature         *float64          `json:"temperature"`
	TopP                *float64          `json:"top_p"`
	FrequencyPenalty    *float64          `json:"frequency_penalty"`
	PresencePenalty     *float64          `json:"presence_penalty"`
	Seed                json.RawMessage   `json:"seed"`
	Stop                json.RawMessage   `json:"stop"`
	N                   *int              `json:"n"`
	ResponseFormat      json.RawMessage   `json:"response_format"`
	ServiceTier         *string           `json:"service_tier"`
}

type contentPart struct {
	Type     string  `json:"type"`
	Text     *string `json:"text"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	} `json:"image_url"`
}

// parseTextParts extracts plain text and image URLs from a content payload.
// allowImages controls whether image_url parts are accepted (current user turn
// only); history turns are text-only. Missing content coerces to empty text,
// matching explicit null: assistant tool-call turns commonly omit content or
// send null. Unknown part fields are ignored for compatibility; structural
// mismatches still fail.
func parseTextParts(raw json.RawMessage, allowImages bool) (string, []string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, true
	}
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return "", nil, false
		}
		return text, nil, true
	}
	if len(raw) == 0 || raw[0] != '[' {
		return "", nil, false
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 || len(parts) > 32 {
		return "", nil, false
	}
	var text strings.Builder
	var images []string
	for _, part := range parts {
		switch part.Type {
		case "text":
			if part.Text == nil || part.ImageURL != nil {
				return "", nil, false
			}
			text.WriteString(*part.Text)
		case "image_url":
			if !allowImages || part.ImageURL == nil || part.Text != nil {
				return "", nil, false
			}
			url := strings.TrimSpace(part.ImageURL.URL)
			if url == "" || len(url) > (8<<20) {
				return "", nil, false
			}
			images = append(images, url)
		default:
			return "", nil, false
		}
	}
	return text.String(), images, true
}

func parseClientTools(raw []json.RawMessage) ([]llm.Tool, bool) {
	if len(raw) > 64 {
		return nil, false
	}
	tools := make([]llm.Tool, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		if len(item) == 0 || len(bytes.TrimSpace(item)) == 0 || len(item) > 64<<10 {
			return nil, false
		}
		// Permissive envelope: unknown fields such as "strict" are ignored
		// for compatibility with evolving client SDKs.
		var wire struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(item, &wire) != nil {
			return nil, false
		}
		if wire.Type != "function" {
			return nil, false
		}
		name := strings.TrimSpace(wire.Function.Name)
		if !clientToolNameRE.MatchString(name) || seen[name] {
			return nil, false
		}
		seen[name] = true
		description := strings.TrimSpace(wire.Function.Description)
		if len([]rune(description)) > 16000 || strings.ContainsRune(description, '\x00') {
			return nil, false
		}
		params := bytes.TrimSpace(wire.Function.Parameters)
		var parsed llm.ToolParameters
		if len(params) == 0 || string(params) == "null" {
			parsed = llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{}}
		} else {
			// Permissive schema: unknown JSON-Schema keywords are dropped
			// rather than rejecting the whole tool.
			if json.Unmarshal(params, &parsed) != nil || parsed.Type != "object" {
				return nil, false
			}
			if parsed.Properties == nil {
				parsed.Properties = map[string]llm.ToolParameterProperty{}
			}
		}
		tools = append(tools, llm.Tool{Type: "function", Function: llm.ToolDefinition{Name: name, Description: description, Parameters: parsed}})
	}
	return tools, true
}

func parseToolChoice(raw json.RawMessage) (agent.ClientToolChoice, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return agent.ClientToolChoice{}, true
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var choice string
		if json.Unmarshal(trimmed, &choice) != nil {
			return agent.ClientToolChoice{}, false
		}
		switch choice {
		case "", "auto":
			return agent.ClientToolChoice{Mode: "auto"}, true
		case "none", "required":
			return agent.ClientToolChoice{Mode: choice}, true
		}
		return agent.ClientToolChoice{}, false
	}
	var typeProbe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(trimmed, &typeProbe) != nil {
		return agent.ClientToolChoice{}, false
	}
	switch typeProbe.Type {
	case "function":
		var wire struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(trimmed, &wire) != nil || wire.Type != "function" || !clientToolNameRE.MatchString(strings.TrimSpace(wire.Function.Name)) {
			return agent.ClientToolChoice{}, false
		}
		return agent.ClientToolChoice{Mode: "named", Named: strings.TrimSpace(wire.Function.Name)}, true
	case "allowed_tools":
		var wire struct {
			Type         string `json:"type"`
			AllowedTools struct {
				Mode  string            `json:"mode"`
				Tools []json.RawMessage `json:"tools"`
			} `json:"allowed_tools"`
		}
		if json.Unmarshal(trimmed, &wire) != nil || wire.Type != "allowed_tools" {
			return agent.ClientToolChoice{}, false
		}
		if wire.AllowedTools.Mode != "auto" && wire.AllowedTools.Mode != "required" {
			return agent.ClientToolChoice{}, false
		}
		if len(wire.AllowedTools.Tools) == 0 || len(wire.AllowedTools.Tools) > 64 {
			return agent.ClientToolChoice{}, false
		}
		names := make([]string, 0, len(wire.AllowedTools.Tools))
		seen := make(map[string]bool, len(wire.AllowedTools.Tools))
		for _, item := range wire.AllowedTools.Tools {
			if len(item) == 0 || len(item) > 4<<10 {
				return agent.ClientToolChoice{}, false
			}
			var entry struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(item, &entry) != nil || entry.Type != "function" || !clientToolNameRE.MatchString(strings.TrimSpace(entry.Function.Name)) {
				return agent.ClientToolChoice{}, false
			}
			name := strings.TrimSpace(entry.Function.Name)
			if seen[name] {
				return agent.ClientToolChoice{}, false
			}
			seen[name] = true
			names = append(names, name)
		}
		return agent.ClientToolChoice{Mode: "allowed", Allowed: names, AllowedMode: wire.AllowedTools.Mode}, true
	default:
		return agent.ClientToolChoice{}, false
	}
}

// parseStopSequences normalizes the OpenAI stop field (a string or an array
// of up to four strings) into a sequence list. Null/missing means no stops;
// empty sequences are dropped, and an all-empty input means no stops.
func parseStopSequences(raw json.RawMessage) ([]string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, true
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var sequence string
		if json.Unmarshal(trimmed, &sequence) != nil || len(sequence) > 4096 {
			return nil, false
		}
		if sequence == "" {
			return nil, true
		}
		return []string{sequence}, true
	}
	var sequences []string
	if json.Unmarshal(trimmed, &sequences) != nil || len(sequences) > 4 {
		return nil, false
	}
	kept := sequences[:0]
	for _, sequence := range sequences {
		if len(sequence) > 4096 {
			return nil, false
		}
		if sequence != "" {
			kept = append(kept, sequence)
		}
	}
	if len(kept) == 0 {
		return nil, true
	}
	return kept, true
}

// parseResponseFormat normalizes the OpenAI response_format field. Only the
// text and json_object shapes are supported; json_schema needs output-schema
// enforcement the provider loop does not implement, so it fails here.
func parseResponseFormat(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", true
	}
	var wire struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(trimmed, &wire) != nil {
		return "", false
	}
	switch wire.Type {
	case "text":
		return "", true
	case "json_object":
		return "json_object", true
	default:
		return "", false
	}
}

// parseSeed normalizes the OpenAI seed field to an integer. Floats and
// out-of-range values fail as unsupported options rather than malformed
// JSON: the envelope is valid, only the value is unusable.
func parseSeed(raw json.RawMessage) (*int64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, true
	}
	var seed int64
	if json.Unmarshal(trimmed, &seed) != nil {
		return nil, false
	}
	// Reject floats such as 7.5, which decode into int64 targets only by
	// truncation on some parsers; encoding/json rejects them outright, but
	// re-marshaling guards against exponential forms losing precision.
	var check float64
	if json.Unmarshal(trimmed, &check) != nil || check != math.Trunc(check) {
		return nil, false
	}
	return &seed, true
}

func inRange(value *float64, min, max float64) bool {
	return value == nil || (!math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= min && *value <= max)
}

func parseWireToolCalls(calls []wireToolCall) ([]llm.ToolCall, bool) {
	if len(calls) == 0 {
		return nil, true
	}
	if len(calls) > 16 {
		return nil, false
	}
	out := make([]llm.ToolCall, 0, len(calls))
	seen := map[string]bool{}
	for _, call := range calls {
		id := strings.TrimSpace(call.ID)
		if id == "" || len(id) > 128 || seen[id] {
			return nil, false
		}
		seen[id] = true
		if call.Type != "" && call.Type != "function" {
			return nil, false
		}
		name := strings.TrimSpace(call.Function.Name)
		if !clientToolNameRE.MatchString(name) {
			return nil, false
		}
		argsRaw := strings.TrimSpace(call.Function.Arguments)
		if argsRaw == "" {
			argsRaw = "{}"
		}
		if len(argsRaw) > 32<<10 {
			return nil, false
		}
		var args map[string]interface{}
		decoder := json.NewDecoder(strings.NewReader(argsRaw))
		decoder.UseNumber()
		if decoder.Decode(&args) != nil {
			return nil, false
		}
		if args == nil {
			args = map[string]interface{}{}
		}
		out = append(out, llm.ToolCall{ID: id, Function: llm.ToolFunction{Name: name, Arguments: args, RawArguments: argsRaw}})
	}
	return out, true
}

// fetchImageURL converts one image_url value to a normalized model input.
// Data URLs decode locally; remote URLs fetch with bounded size and time.
func fetchImageURL(ctx context.Context, url string) (llm.InputImage, bool) {
	url = strings.TrimSpace(url)
	if strings.HasPrefix(url, "data:") {
		comma := strings.Index(url, ",")
		if comma < 0 {
			return llm.InputImage{}, false
		}
		header, payload := url[:comma], url[comma+1:]
		if !strings.Contains(header, ";base64") {
			return llm.InputImage{}, false
		}
		mime := strings.TrimPrefix(header, "data:")
		if idx := strings.Index(mime, ";"); idx >= 0 {
			mime = mime[:idx]
		}
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
		if err != nil {
			if data, err = base64.URLEncoding.DecodeString(strings.TrimSpace(payload)); err != nil {
				return llm.InputImage{}, false
			}
		}
		if len(data) == 0 || len(data) > media.MaxImageBytes {
			return llm.InputImage{}, false
		}
		result, err := media.NormalizeInputImageFromBytes(nil, mime, data, "api")
		if err != nil {
			return llm.InputImage{}, false
		}
		return result.Image, true
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return llm.InputImage{}, false
	}
	if err := ctx.Err(); err != nil {
		return llm.InputImage{}, false
	}
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return llm.InputImage{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return llm.InputImage{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return llm.InputImage{}, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxImageBytes+1))
	if err != nil || len(data) == 0 || len(data) > media.MaxImageBytes {
		return llm.InputImage{}, false
	}
	result, err := media.NormalizeInputImageFromBytes(resp.Header, "", data, "api")
	if err != nil {
		return llm.InputImage{}, false
	}
	return result.Image, true
}

const toolContinuationPrompt = "Continue with the completed tool results above to answer the user's last question."

func (g *Gateway) completions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		g.reject(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}
	principal, ok := g.resolveKeyPrincipal(w, r)
	if !ok {
		return
	}
	ct := r.Header.Get("Content-Type")
	if ct != "application/json" && !strings.HasPrefix(ct, "application/json;") {
		g.reject(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 || len(body) > maxBody {
		g.reject(w, http.StatusBadRequest, "invalid_json", "Invalid or unsupported JSON request.")
		return
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		g.reject(w, http.StatusBadRequest, "invalid_json", "Invalid or unsupported JSON request.")
		return
	}
	for key := range top {
		switch key {
		case "model", "messages", "stream", "stream_options", "max_tokens", "max_completion_tokens", "tools", "tool_choice", "parallel_tool_calls", "response_format", "service_tier":
		case "temperature", "top_p", "frequency_penalty", "presence_penalty", "seed", "stop", "n":
		case "top_k", "repetition_penalty", "user", "logit_bias", "logprobs", "top_logprobs", "reasoning_effort", "store":
			delete(top, key)
		default:
			g.reject(w, http.StatusBadRequest, "invalid_json", "Invalid or unsupported JSON request.")
			return
		}
	}
	filtered, err := json.Marshal(top)
	if err != nil {
		g.reject(w, http.StatusBadRequest, "invalid_json", "Invalid or unsupported JSON request.")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(filtered))
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
	if input.MaxTokens != nil && *input.MaxTokens <= 0 {
		g.reject(w, http.StatusBadRequest, "unsupported_options", "Unsupported client generation options.")
		return
	}
	if input.MaxCompletionTokens != nil && *input.MaxCompletionTokens <= 0 {
		g.reject(w, http.StatusBadRequest, "unsupported_options", "Unsupported client generation options.")
		return
	}
	clientTools, valid := parseClientTools(input.Tools)
	if !valid {
		g.reject(w, http.StatusBadRequest, "unsupported_options", "Invalid client tool definition.")
		return
	}
	toolChoice, valid := parseToolChoice(input.ToolChoice)
	if !valid {
		g.reject(w, http.StatusBadRequest, "unsupported_options", "Unsupported client tool choice.")
		return
	}
	// Named and allowed-tools existence is validated against the merged
	// Oswald and client catalog inside the agent, which alone knows the
	// profile's builtin tools. Syntax errors fail here; unknown names fail
	// there as invalid_tool_choice.
	if !inRange(input.Temperature, 0, 2) || !inRange(input.TopP, 0, 1) ||
		!inRange(input.FrequencyPenalty, -2, 2) || !inRange(input.PresencePenalty, -2, 2) {
		g.reject(w, http.StatusBadRequest, "unsupported_options", "Unsupported client generation options.")
		return
	}
	stop, valid := parseStopSequences(input.Stop)
	if !valid {
		g.reject(w, http.StatusBadRequest, "unsupported_options", "Unsupported client generation options.")
		return
	}
	seed, valid := parseSeed(input.Seed)
	if !valid {
		g.reject(w, http.StatusBadRequest, "unsupported_options", "Unsupported client generation options.")
		return
	}
	if input.N != nil && *input.N != 1 {
		g.reject(w, http.StatusBadRequest, "unsupported_options", "Only a single completion choice is supported.")
		return
	}
	format, valid := parseResponseFormat(input.ResponseFormat)
	if !valid {
		g.reject(w, http.StatusBadRequest, "unsupported_options", "Unsupported response format.")
		return
	}
	tier := ""
	if input.ServiceTier != nil {
		switch *input.ServiceTier {
		case "auto", "default", "flex", "scale", "priority", "fast":
			// Single-tier deployment: the requested tier is acknowledged
			// verbatim in completion responses without changing routing.
			tier = *input.ServiceTier
		default:
			g.reject(w, http.StatusBadRequest, "unsupported_options", "Unsupported service tier.")
			return
		}
	}
	var generation *agent.ClientGeneration
	if input.Temperature != nil || input.TopP != nil || input.FrequencyPenalty != nil ||
		input.PresencePenalty != nil || seed != nil || stop != nil ||
		input.MaxTokens != nil || input.MaxCompletionTokens != nil || format != "" {
		cap := input.MaxTokens
		if cap == nil {
			cap = input.MaxCompletionTokens
		}
		generation = &agent.ClientGeneration{
			Temperature:      input.Temperature,
			TopP:             input.TopP,
			FrequencyPenalty: input.FrequencyPenalty,
			PresencePenalty:  input.PresencePenalty,
			Seed:             seed,
			Stop:             stop,
			MaxTokens:        cap,
			ResponseFormat:   format,
		}
	}
	if len(input.Messages) == 0 {
		g.reject(w, http.StatusBadRequest, "invalid_messages", "Messages must contain at least 1 turn.")
		return
	}
	history := make([]llm.ChatMessage, 0, len(input.Messages)-1)
	var currentImages []llm.InputImage
	continuation := input.Messages[len(input.Messages)-1].Role == "tool"
	for i, msg := range input.Messages {
		last := i == len(input.Messages)-1
		switch msg.Role {
		case "user", "assistant", "system", "developer", "tool":
		default:
			g.reject(w, http.StatusBadRequest, "unsupported_role", "Unsupported message role.")
			return
		}
		if last && msg.Role != "user" && msg.Role != "tool" {
			g.reject(w, http.StatusBadRequest, "unsupported_role", "Messages must end in a user or tool message.")
			return
		}
		if msg.ReasoningContent != nil && msg.Role != "assistant" {
			g.reject(w, http.StatusBadRequest, "unsupported_content", "Reasoning content is only supported on prior assistant messages.")
			return
		}
		if msg.ReasoningContent != nil && len(*msg.ReasoningContent) > 128<<10 {
			g.reject(w, http.StatusBadRequest, "unsupported_content", "Reasoning content is too large.")
			return
		}
		name, valid := parseParticipantName(msg)
		if !valid {
			g.reject(w, http.StatusBadRequest, "unsupported_content", "Invalid participant name.")
			return
		}
		if msg.Role == "tool" {
			if msg.ToolCallID == nil || strings.TrimSpace(*msg.ToolCallID) == "" || len(strings.TrimSpace(*msg.ToolCallID)) > 128 {
				g.reject(w, http.StatusBadRequest, "unsupported_content", "Tool messages require a tool call id.")
				return
			}
			if len(msg.ToolCalls) != 0 {
				g.reject(w, http.StatusBadRequest, "unsupported_content", "Tool messages cannot contain tool calls.")
				return
			}
			text, _, valid := parseTextParts(msg.Content, false)
			if !valid {
				g.reject(w, http.StatusBadRequest, "unsupported_content", "Only text strings or arrays of text parts are supported.")
				return
			}
			name := ""
			history = append(history, llm.ChatMessage{Role: "tool", Content: text, ToolName: name, ToolCallID: strings.TrimSpace(*msg.ToolCallID)})
			continue
		}
		calls, valid := parseWireToolCalls(msg.ToolCalls)
		if !valid {
			g.reject(w, http.StatusBadRequest, "unsupported_content", "Invalid tool calls.")
			return
		}
		if len(calls) > 0 && msg.Role != "assistant" {
			g.reject(w, http.StatusBadRequest, "unsupported_content", "Tool calls are only supported on assistant messages.")
			return
		}
		text, imageURLs, valid := parseTextParts(msg.Content, last && msg.Role == "user")
		if !valid {
			g.reject(w, http.StatusBadRequest, "unsupported_content", "Only text strings or arrays of text parts are supported.")
			return
		}
		if msg.Role == "assistant" && strings.TrimSpace(text) == "" && len(calls) == 0 {
			g.reject(w, http.StatusBadRequest, "unsupported_content", "Assistant messages require content or tool calls.")
			return
		}
		if last {
			if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), "/") {
				g.reject(w, http.StatusBadRequest, "invalid_prompt", "A nonempty, non-command user message is required.")
				return
			}
			if len(imageURLs) > media.MaxImagesPerRequest {
				g.reject(w, http.StatusBadRequest, "history_limit", "Too many images in the current message.")
				return
			}
			for _, url := range imageURLs {
				image, ok := fetchImageURL(r.Context(), url)
				if !ok {
					g.reject(w, http.StatusBadRequest, "unsupported_content", "Image could not be loaded.")
					return
				}
				currentImages = append(currentImages, image)
			}
			deps := g.deps
			if g.broker != nil {
				deps.Broker = g.broker
			}
			if deps.Log == nil {
				deps.Log = g.log
			}
			id := config.NewRequestID()
			includeUsage := input.StreamOptions != nil && input.StreamOptions.IncludeUsage
			responder := &responseWriter{w: w, ctx: r.Context(), id: "chatcmpl-" + id, stream: input.Stream, includeUsage: includeUsage}
			var streamFunc func(agent.StreamChunk)
			if input.Stream {
				streamFunc = responder.Stream
			}
			outcome := g.execute(gatewayruntime.Request{ReceivedAt: time.Now(), RequestID: id, ChatID: "openai:" + id, SessionKey: "openai:" + id, Principal: principal, IsDirect: true, IsMention: true, Text: text, PublicUserText: text, ClientHistory: history, ClientTools: clientTools, ClientToolChoice: toolChoice, ClientParallelToolCalls: input.ParallelToolCalls, ClientGeneration: generation, ServiceTier: tier, Images: currentImages, Stateless: true, Context: r.Context(), StreamFunc: streamFunc}, deps, responder)
			if !responder.sent && r.Context().Err() == nil {
				if outcome.Action == routing.ActionIgnore && outcome.Reason == "user_banned" {
					w.WriteHeader(http.StatusNoContent)
				} else {
					writeError(w, http.StatusServiceUnavailable, "server_error", "Request could not be completed.")
				}
			}
			return
		}
		thinking := ""
		if msg.ReasoningContent != nil {
			thinking = *msg.ReasoningContent
		}
		// Prior reasoning is retained as quoted, lower-authority history
		// reference (Ollama thinking parity), never replayed as model
		// reasoning. Participant names are retained as quoted history
		// metadata. The current-turn name carries no attribution: the sender
		// is always the API key owner.
		history = append(history, llm.ChatMessage{Role: msg.Role, Content: text, Name: name, ToolCalls: calls, Thinking: thinking})
	}
	if !continuation {
		g.reject(w, http.StatusBadRequest, "unsupported_role", "Messages must end in a user or tool message.")
		return
	}
	// Tool continuation: the client posted assistant tool calls with their
	// results and no new user turn. The full exchange is already in history;
	// drive the model with a fixed neutral prompt.
	deps := g.deps
	if g.broker != nil {
		deps.Broker = g.broker
	}
	if deps.Log == nil {
		deps.Log = g.log
	}
	id := config.NewRequestID()
	includeUsage := input.StreamOptions != nil && input.StreamOptions.IncludeUsage
	responder := &responseWriter{w: w, ctx: r.Context(), id: "chatcmpl-" + id, stream: input.Stream, includeUsage: includeUsage}
	var streamFunc func(agent.StreamChunk)
	if input.Stream {
		streamFunc = responder.Stream
	}
	outcome := g.execute(gatewayruntime.Request{ReceivedAt: time.Now(), RequestID: id, ChatID: "openai:" + id, SessionKey: "openai:" + id, Principal: principal, IsDirect: true, IsMention: true, Text: toolContinuationPrompt, PublicUserText: "", ClientHistory: history, ClientTools: clientTools, ClientToolChoice: toolChoice, ClientParallelToolCalls: input.ParallelToolCalls, ClientGeneration: generation, ServiceTier: tier, Stateless: true, Context: r.Context(), StreamFunc: streamFunc}, deps, responder)
	if !responder.sent && r.Context().Err() == nil {
		if outcome.Action == routing.ActionIgnore && outcome.Reason == "user_banned" {
			w.WriteHeader(http.StatusNoContent)
		} else {
			writeError(w, http.StatusServiceUnavailable, "server_error", "Request could not be completed.")
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, code int, kind, message string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": kind}})
}
