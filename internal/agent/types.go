package agent

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

// ModelMetrics holds performance data from a single LLM call.
type ModelMetrics struct {
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	TotalTokens      int     `json:"total_tokens,omitempty"`
	DurationMS       int64   `json:"duration_ms,omitempty"`
	TokensPerSecond  float64 `json:"tokens_per_second"`
}

// Response is the final payload returned to the gateway after processing.
type Response struct {
	Kind               string `json:"-"`
	PersistenceStatus  string `json:"-"`
	ToolExecutionCount int    `json:"-"`
	ToolBlockedCount   int    `json:"-"`
	// ToolChoiceError carries the fixed invalid_tool_choice code
	// ("named_tool_missing" or "no_tools_available"). It is separate from
	// Error so runtime telemetry keeps provider failures distinct.
	ToolChoiceError string `json:"-"`
	// FinishReason carries the terminal provider stop reason for text
	// answers ("stop" or "length"). Empty behaves as "stop"; tool_calls
	// answers are reported separately via ToolCalls.
	FinishReason string `json:"-"`
	// ServiceTier echoes the caller's validated service_tier request value
	// in completion responses. Empty omits it.
	ServiceTier string `json:"-"`
	Model       string `json:"model"`
	Response    string `json:"response,omitempty"`
	Thinking    string `json:"thinking,omitempty"` // reasoning tokens emitted before the response
	// ToolCalls holds caller-owned tool calls to return verbatim. When non-empty,
	// the gateway reports finish_reason tool_calls and omits server execution.
	ToolCalls []llm.ToolCall `json:"-"`
	// Usage totals accumulate observed provider-reported tokens across all
	// model calls in this request. UsageReported is false when no call
	// reported usage, in which case gateways must omit usage rather than
	// invent it. UsageComplete is false when some submissions never reported.
	UsagePromptTokens     int                      `json:"-"`
	UsageCompletionTokens int                      `json:"-"`
	UsageTotalTokens      int                      `json:"-"`
	UsageReported         bool                     `json:"-"`
	UsageComplete         bool                     `json:"-"`
	Error                 string                   `json:"error,omitempty"`
	Metrics               *ModelMetrics            `json:"metrics,omitempty"`
	Attachments           []media.OutputAttachment `json:"-"`

	SourceTurnID      int64 `json:"-"`
	SessionGeneration int   `json:"-"`
}

// Request contains one fully resolved request submitted to the agent.
type Request struct {
	RequestID   string
	Principal   identity.Principal
	DisplayName string
	// ChatLabel names the transport conversation for the session-context block
	// (for example `"DM with fragsap"`). It is untrusted metadata and empty for
	// stateless requests.
	ChatLabel  string
	SessionKey string
	IsDirect   bool
	Prompt     string
	// PlatformMessageID is the untrusted inbound transport message identifier,
	// persisted on the user message row when present.
	PlatformMessageID string
	// ChatID is the untrusted transport conversation identifier stored on the
	// session row; ChatDisplayName is its plain conversation name for the
	// session origin record.
	ChatID, ChatDisplayName string
	// TransportProfile names the profile owning the receiving adapter,
	// credential, and allowlist. It never affects session ownership.
	TransportProfile string
	// Stateless prevents session reads/writes and uses ClientHistory as untrusted context.
	Stateless     bool
	ClientHistory []llm.ChatMessage
	// ClientTools are caller-supplied function definitions for this request only.
	ClientTools []llm.Tool
	// ClientToolChoice is the normalized caller preference. Mode is one of
	// "", "none", "auto", "required", "named", or "allowed". Named carries a
	// single forced tool name; Allowed carries a filtered tool-name set with
	// AllowedMode "auto" or "required".
	ClientToolChoice ClientToolChoice
	// ClientParallelToolCalls carries the caller's explicit
	// parallel_tool_calls preference. Nil leaves the provider default;
	// single-tool forcing disables parallel calls unless overridden here.
	ClientParallelToolCalls *bool
	// ClientGeneration carries the caller's sampling and stop parameters
	// for stateless requests. Nil leaves provider defaults untouched.
	ClientGeneration *ClientGeneration
	// ServiceTier carries the caller's validated service_tier value for
	// stateless requests. Empty omits it from responses.
	ServiceTier string
	Images      []llm.InputImage
	StreamFunc  func(StreamChunk)
}

// ClientToolChoice constrains the merged Oswald and caller tool catalog for
// one stateless API request. It mirrors the OpenAI chat completions
// tool_choice shape: string modes, a named function force, and the
// allowed_tools allow-list.
type ClientToolChoice struct {
	// Mode is "", "none", "auto", "required", "named", or "allowed".
	// Empty behaves as "auto".
	Mode string
	// Named names the single forced tool when Mode is "named".
	Named string
	// Allowed lists tool names to offer when Mode is "allowed".
	Allowed []string
	// AllowedMode is "auto" or "required" when Mode is "allowed".
	AllowedMode string
}

// IsNone reports whether the caller requested no tool calls at all.
func (c ClientToolChoice) IsNone() bool { return c.Mode == "none" }

// ClientGeneration carries caller-supplied generation parameters for one
// stateless API request. All fields are optional; nil leaves the provider
// default in place. Ranges mirror the OpenAI chat completions contract:
// temperature 0–2, top_p 0–1, frequency/presence penalties −2–2, seed any
// integer, stop up to four sequences, max tokens a positive cap applied to
// each provider call in the request loop.
type ClientGeneration struct {
	Temperature      *float64
	TopP             *float64
	FrequencyPenalty *float64
	PresencePenalty  *float64
	Seed             *int64
	Stop             []string
	MaxTokens        *int
	// ResponseFormat is "" or "json_object"; richer schemas are rejected
	// at the gateway because the provider loop cannot enforce them.
	ResponseFormat string
}

// setObservedUsage records provider-reported request totals on the response.
// Gateways omit usage unless UsageReported, so unreported turns never invent it.
func (r *Response) setObservedUsage(s requestctx.UsageSnapshot) {
	if r == nil {
		return
	}
	r.UsagePromptTokens, r.UsageCompletionTokens, r.UsageTotalTokens = s.PromptTokens, s.CompletionTokens, s.TotalTokens
	r.UsageReported = s.UsageReportedCallCount > 0
	r.UsageComplete = r.UsageReported && s.ModelSubmissionCount > 0 && s.UsageCompleteCallCount == s.ModelSubmissionCount
}

// mapMetrics converts an LLM response into a model metrics summary.
func mapMetrics(resp *llm.ChatResponse) *ModelMetrics {
	if resp == nil {
		return nil
	}
	tps := 0.0
	if resp.DurationMS > 0 && resp.CompletionTokens > 0 {
		tps = float64(resp.CompletionTokens) / (float64(resp.DurationMS) / 1000)
	}
	return &ModelMetrics{
		Model:            resp.Model,
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		TotalTokens:      resp.TotalTokens,
		DurationMS:       resp.DurationMS,
		TokensPerSecond:  tps,
	}
}
