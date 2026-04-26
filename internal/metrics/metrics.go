package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics collects the small Prometheus surface used by the Grafana graphs.
type Metrics struct {
	requestsTotal           *prometheus.CounterVec
	requestDuration         *prometheus.HistogramVec
	userRequestsTotal       *prometheus.CounterVec
	toolCallsTotal          *prometheus.CounterVec
	requestPromptTokens     *prometheus.HistogramVec
	requestCompletionTokens *prometheus.HistogramVec
}

// New registers and returns the shared metrics collector set.
func New() *Metrics {
	tokenBuckets := []float64{0, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768}

	return &Metrics{
		requestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "oswald_requests_total",
			Help: "Total agent-bound requests by gateway, request kind, and result.",
		}, []string{"gateway", "kind", "result"}),
		requestDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "oswald_request_duration_seconds",
			Help:    "End-to-end duration of agent-bound requests.",
			Buckets: prometheus.DefBuckets,
		}, []string{"gateway", "kind", "result"}),
		userRequestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "oswald_user_requests_total",
			Help: "Total agent-bound requests by gateway and canonical user ID.",
		}, []string{"gateway", "user_id"}),
		toolCallsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "oswald_tool_calls_total",
			Help: "Total tool executions by gateway, tool, and result.",
		}, []string{"gateway", "tool", "result"}),
		requestPromptTokens: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "oswald_request_prompt_tokens",
			Help:    "Total prompt tokens consumed across all LLM calls for a request.",
			Buckets: tokenBuckets,
		}, []string{"gateway", "kind"}),
		requestCompletionTokens: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "oswald_request_completion_tokens",
			Help:    "Total completion tokens generated across all LLM calls for a request.",
			Buckets: tokenBuckets,
		}, []string{"gateway", "kind"}),
	}
}

// Handler returns the Prometheus scrape handler.
func (m *Metrics) Handler() http.Handler { return promhttp.Handler() }

// ObserveRequest records the final outcome of an agent-bound request.
func (m *Metrics) ObserveRequest(gateway, kind, userID, result string, duration time.Duration) {
	if m == nil {
		return
	}
	if kind == "" {
		kind = "unknown"
	}
	m.requestsTotal.WithLabelValues(gateway, kind, result).Inc()
	m.requestDuration.WithLabelValues(gateway, kind, result).Observe(duration.Seconds())
	if userID != "" {
		m.userRequestsTotal.WithLabelValues(gateway, userID).Inc()
	}
}

// ObserveToolCall records a tool execution performed while serving a request.
func (m *Metrics) ObserveToolCall(gateway, tool, result string) {
	if m == nil {
		return
	}
	m.toolCallsTotal.WithLabelValues(gateway, tool, result).Inc()
}

// ObserveRequestTokens records aggregate prompt and completion tokens for one request.
func (m *Metrics) ObserveRequestTokens(gateway, kind string, promptTokens, completionTokens int) {
	if m == nil {
		return
	}
	if kind == "" {
		kind = "unknown"
	}
	if promptTokens > 0 {
		m.requestPromptTokens.WithLabelValues(gateway, kind).Observe(float64(promptTokens))
	}
	if completionTokens > 0 {
		m.requestCompletionTokens.WithLabelValues(gateway, kind).Observe(float64(completionTokens))
	}
}
