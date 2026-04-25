package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics collects Prometheus instrumentation for request, broker, model, tool,
// gateway, and memory activity.
type Metrics struct {
	requestsTotal            *prometheus.CounterVec
	requestDuration          *prometheus.HistogramVec
	activeRequests           *prometheus.GaugeVec
	userRequestsTotal        *prometheus.CounterVec
	requestInputChars        *prometheus.HistogramVec
	requestOutputChars       *prometheus.HistogramVec
	requestImages            *prometheus.HistogramVec
	requestImageBytes        *prometheus.HistogramVec
	brokerQueueDepth         prometheus.Gauge
	brokerQueueWait          *prometheus.HistogramVec
	brokerRejections         *prometheus.CounterVec
	brokerWorkersActive      prometheus.Gauge
	brokerProcessingDuration *prometheus.HistogramVec
	agentIterations          *prometheus.HistogramVec
	agentToolCallsPerRequest *prometheus.HistogramVec
	agentStreamingRequests   *prometheus.CounterVec
	agentFinalEmptyTotal     *prometheus.CounterVec
	agentToolBudgetStops     prometheus.Counter
	memoryTurnsLoaded        *prometheus.HistogramVec
	memoryTurnsPersisted     *prometheus.HistogramVec
	memoryCompactions        prometheus.Counter
	memoryCompactedPairs     prometheus.Histogram
	memoryPromptEstimate     *prometheus.HistogramVec
	memoryActualPrompt       *prometheus.HistogramVec
	memoryBudgetUtilization  *prometheus.HistogramVec
	memoryOverBudgetTotal    *prometheus.CounterVec
	memorySessionsCurrent    prometheus.Gauge
	memoryTurnsCurrent       prometheus.Gauge
	llmCallsTotal            *prometheus.CounterVec
	llmCallDuration          *prometheus.HistogramVec
	llmTimeToFirstToken      *prometheus.HistogramVec
	llmPromptTokens          *prometheus.HistogramVec
	llmEvalTokens            *prometheus.HistogramVec
	llmTokensPerSecond       *prometheus.HistogramVec
	llmLoadDuration          *prometheus.HistogramVec
	llmPromptEvalDuration    *prometheus.HistogramVec
	llmEvalDuration          *prometheus.HistogramVec
	llmDoneReasonTotal       *prometheus.CounterVec
	llmToolCallMessagesTotal *prometheus.CounterVec
	toolsTotal               *prometheus.CounterVec
	toolFailuresTotal        *prometheus.CounterVec
	toolDuration             *prometheus.HistogramVec
	toolArgsSize             *prometheus.HistogramVec
	toolResultSize           *prometheus.HistogramVec
	gatewayReceivedTotal     *prometheus.CounterVec
	gatewayIgnoredTotal      *prometheus.CounterVec
	gatewaySentTotal         *prometheus.CounterVec
	gatewaySendDuration      *prometheus.HistogramVec
	gatewaySendFailures      *prometheus.CounterVec
	unsupportedAttachments   *prometheus.CounterVec
	attachmentDeclaredMIME   *prometheus.CounterVec
	attachmentDetectedMIME   *prometheus.CounterVec
	websocketConnections     prometheus.Gauge
	websocketStreamChunks    *prometheus.CounterVec
	websocketWriteFailures   prometheus.Counter
	discordTypingRequests    *prometheus.CounterVec
	discordReplyContext      *prometheus.CounterVec
	discordSplitChunks       prometheus.Histogram
	imessageWebhooks         *prometheus.CounterVec
	imessageTypingRequests   *prometheus.CounterVec
	imessageContactLookups   *prometheus.CounterVec
	imessageReplyContext     *prometheus.CounterVec
	errorsTotal              *prometheus.CounterVec
}

// New registers and returns the shared metrics collector set.
func New() *Metrics {
	requestSizeBuckets := []float64{0, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384}
	tokenBuckets := []float64{64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768}
	evalBuckets := []float64{8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096}
	throughputBuckets := []float64{1, 5, 10, 20, 40, 80, 120, 160, 240, 320}
	utilizationBuckets := []float64{0.1, 0.25, 0.5, 0.75, 0.9, 1.0, 1.1, 1.25, 1.5, 2.0}

	return &Metrics{
		requestsTotal:            promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_requests_total", Help: "Total agent-bound requests by gateway and outcome."}, []string{"gateway", "result"}),
		requestDuration:          promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_request_duration_seconds", Help: "End-to-end request duration from broker enqueue to agent completion.", Buckets: prometheus.DefBuckets}, []string{"gateway", "result"}),
		activeRequests:           promauto.NewGaugeVec(prometheus.GaugeOpts{Name: "oswald_active_requests", Help: "Current in-flight requests by gateway."}, []string{"gateway"}),
		userRequestsTotal:        promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_user_requests_total", Help: "Total requests by gateway and canonical user ID."}, []string{"gateway", "user_id"}),
		requestInputChars:        promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_request_input_chars", Help: "Request prompt size in characters.", Buckets: requestSizeBuckets}, []string{"gateway"}),
		requestOutputChars:       promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_request_output_chars", Help: "Final response size in characters.", Buckets: requestSizeBuckets}, []string{"gateway"}),
		requestImages:            promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_request_images_total", Help: "Images attached to a request.", Buckets: []float64{0, 1, 2, 3, 4, 5}}, []string{"gateway"}),
		requestImageBytes:        promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_request_image_bytes", Help: "Total image bytes attached to a request.", Buckets: []float64{0, 1024, 10 * 1024, 100 * 1024, 512 * 1024, 1024 * 1024, 5 * 1024 * 1024, 10 * 1024 * 1024, 20 * 1024 * 1024, 40 * 1024 * 1024}}, []string{"gateway"}),
		brokerQueueDepth:         promauto.NewGauge(prometheus.GaugeOpts{Name: "oswald_broker_queue_depth", Help: "Current broker queue depth."}),
		brokerQueueWait:          promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_broker_queue_wait_seconds", Help: "Time spent waiting in the broker queue before a worker starts processing.", Buckets: prometheus.DefBuckets}, []string{"gateway"}),
		brokerRejections:         promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_broker_rejections_total", Help: "Total broker rejections by gateway and reason."}, []string{"gateway", "reason"}),
		brokerWorkersActive:      promauto.NewGauge(prometheus.GaugeOpts{Name: "oswald_broker_workers_active", Help: "Current broker workers actively processing a request."}),
		brokerProcessingDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_broker_processing_duration_seconds", Help: "Time spent processing requests on broker workers.", Buckets: prometheus.DefBuckets}, []string{"gateway", "result"}),
		agentIterations:          promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_agent_iterations_per_request", Help: "LLM/tool loop iterations per request.", Buckets: []float64{1, 2, 3, 4, 5, 8, 13, 21}}, []string{"gateway"}),
		agentToolCallsPerRequest: promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_agent_tool_calls_per_request", Help: "Tool calls executed per request.", Buckets: []float64{0, 1, 2, 3, 5, 8, 13, 21}}, []string{"gateway"}),
		agentStreamingRequests:   promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_agent_streaming_requests_total", Help: "Requests processed with streaming enabled or disabled."}, []string{"gateway", "stream"}),
		agentFinalEmptyTotal:     promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_agent_final_response_empty_total", Help: "Requests that finished with an empty final response."}, []string{"gateway"}),
		agentToolBudgetStops:     promauto.NewCounter(prometheus.CounterOpts{Name: "oswald_agent_tool_failure_streak_terminations_total", Help: "Requests where tool failure retry budget was exhausted."}),
		memoryTurnsLoaded:        promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_memory_turns_loaded", Help: "Retained turn pairs loaded from session memory.", Buckets: []float64{0, 1, 2, 3, 5, 8, 13, 21, 34}}, []string{"gateway"}),
		memoryTurnsPersisted:     promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_memory_turns_persisted", Help: "Turn pairs persisted back to session memory per request.", Buckets: []float64{0, 1, 2}}, []string{"gateway"}),
		memoryCompactions:        promauto.NewCounter(prometheus.CounterOpts{Name: "oswald_memory_compactions_total", Help: "Total prompt-history compactions performed."}),
		memoryCompactedPairs:     promauto.NewHistogram(prometheus.HistogramOpts{Name: "oswald_memory_compacted_turn_pairs", Help: "Turn pairs compacted during a compaction event.", Buckets: []float64{1, 2, 3, 5, 8, 13, 21, 34}}),
		memoryPromptEstimate:     promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_memory_prompt_estimate_tokens", Help: "Estimated prompt tokens before model call.", Buckets: tokenBuckets}, []string{"model"}),
		memoryActualPrompt:       promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_memory_actual_prompt_tokens", Help: "Actual prompt tokens reported by Ollama.", Buckets: tokenBuckets}, []string{"model"}),
		memoryBudgetUtilization:  promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_memory_prompt_budget_utilization", Help: "Prompt token utilization as estimate or actual divided by prompt budget.", Buckets: utilizationBuckets}, []string{"model", "kind"}),
		memoryOverBudgetTotal:    promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_memory_history_over_budget_total", Help: "Requests that remained over budget after history compaction."}, []string{"model"}),
		memorySessionsCurrent:    promauto.NewGauge(prometheus.GaugeOpts{Name: "oswald_memory_store_sessions_current", Help: "Current number of in-memory sessions retained."}),
		memoryTurnsCurrent:       promauto.NewGauge(prometheus.GaugeOpts{Name: "oswald_memory_store_turns_current", Help: "Current number of retained turn pairs across all sessions."}),
		llmCallsTotal:            promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_llm_calls_total", Help: "Total Ollama chat calls by model, streaming mode, and outcome."}, []string{"model", "stream", "result"}),
		llmCallDuration:          promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_llm_call_duration_seconds", Help: "Wall-clock duration of Ollama chat calls.", Buckets: prometheus.DefBuckets}, []string{"model", "stream", "result"}),
		llmTimeToFirstToken:      promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_llm_time_to_first_token_seconds", Help: "Wall-clock time to first streamed token or chunk.", Buckets: prometheus.DefBuckets}, []string{"model"}),
		llmPromptTokens:          promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_llm_prompt_tokens", Help: "Prompt token counts returned by Ollama chat calls.", Buckets: tokenBuckets}, []string{"model"}),
		llmEvalTokens:            promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_llm_eval_tokens", Help: "Completion token counts returned by Ollama chat calls.", Buckets: evalBuckets}, []string{"model"}),
		llmTokensPerSecond:       promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_llm_tokens_per_second", Help: "Observed model throughput in tokens per second.", Buckets: throughputBuckets}, []string{"model"}),
		llmLoadDuration:          promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_llm_load_duration_seconds", Help: "Model load duration reported by Ollama.", Buckets: prometheus.DefBuckets}, []string{"model"}),
		llmPromptEvalDuration:    promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_llm_prompt_eval_duration_seconds", Help: "Prompt evaluation duration reported by Ollama.", Buckets: prometheus.DefBuckets}, []string{"model"}),
		llmEvalDuration:          promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_llm_eval_duration_seconds", Help: "Completion evaluation duration reported by Ollama.", Buckets: prometheus.DefBuckets}, []string{"model"}),
		llmDoneReasonTotal:       promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_llm_done_reason_total", Help: "Ollama chat completion reasons by model."}, []string{"model", "reason"}),
		llmToolCallMessagesTotal: promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_llm_tool_call_messages_total", Help: "Model responses that contained tool calls."}, []string{"model"}),
		toolsTotal:               promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_tools_total", Help: "Total tool executions by tool and outcome."}, []string{"tool", "result"}),
		toolFailuresTotal:        promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_tool_failures_total", Help: "Tool execution failures by tool and error type."}, []string{"tool", "error_type"}),
		toolDuration:             promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_tool_duration_seconds", Help: "Tool execution duration by tool and outcome.", Buckets: prometheus.DefBuckets}, []string{"tool", "result"}),
		toolArgsSize:             promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_tool_arguments_size_chars", Help: "Approximate serialized tool argument size.", Buckets: requestSizeBuckets}, []string{"tool"}),
		toolResultSize:           promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_tool_result_size_chars", Help: "Tool result size in characters.", Buckets: requestSizeBuckets}, []string{"tool"}),
		gatewayReceivedTotal:     promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_gateway_messages_received_total", Help: "Total inbound gateway messages observed by gateway and kind."}, []string{"gateway", "kind"}),
		gatewayIgnoredTotal:      promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_gateway_messages_ignored_total", Help: "Total inbound gateway messages ignored by gateway and reason."}, []string{"gateway", "reason"}),
		gatewaySentTotal:         promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_gateway_responses_sent_total", Help: "Total outbound gateway responses by gateway and outcome."}, []string{"gateway", "result"}),
		gatewaySendDuration:      promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "oswald_gateway_send_duration_seconds", Help: "Gateway outbound send duration.", Buckets: prometheus.DefBuckets}, []string{"gateway", "result"}),
		gatewaySendFailures:      promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_gateway_send_failures_total", Help: "Gateway outbound send failures by gateway and reason."}, []string{"gateway", "reason"}),
		unsupportedAttachments:   promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_request_unsupported_attachments_total", Help: "Unsupported or downgraded attachments by gateway and reason."}, []string{"gateway", "reason"}),
		attachmentDeclaredMIME:   promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_request_attachment_declared_mime_total", Help: "Declared attachment MIME types by gateway."}, []string{"gateway", "mime"}),
		attachmentDetectedMIME:   promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_request_attachment_detected_mime_total", Help: "Detected or normalized attachment MIME types by gateway."}, []string{"gateway", "mime"}),
		websocketConnections:     promauto.NewGauge(prometheus.GaugeOpts{Name: "oswald_websocket_connections_current", Help: "Current open WebSocket connections."}),
		websocketStreamChunks:    promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_websocket_stream_chunks_total", Help: "WebSocket stream chunks emitted by chunk type."}, []string{"type"}),
		websocketWriteFailures:   promauto.NewCounter(prometheus.CounterOpts{Name: "oswald_websocket_write_failures_total", Help: "WebSocket write failures."}),
		discordTypingRequests:    promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_discord_typing_requests_total", Help: "Discord typing requests by result."}, []string{"result"}),
		discordReplyContext:      promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_discord_reply_context_total", Help: "Discord reply context lookups by result."}, []string{"result"}),
		discordSplitChunks:       promauto.NewHistogram(prometheus.HistogramOpts{Name: "oswald_discord_split_chunks_total", Help: "Discord message chunks per response.", Buckets: []float64{1, 2, 3, 4, 5, 8, 13}}),
		imessageWebhooks:         promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_imessage_webhooks_total", Help: "iMessage webhooks by type and result."}, []string{"type", "result"}),
		imessageTypingRequests:   promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_imessage_typing_requests_total", Help: "iMessage typing requests by result."}, []string{"result"}),
		imessageContactLookups:   promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_imessage_contact_lookup_total", Help: "iMessage contact lookups by result."}, []string{"result"}),
		imessageReplyContext:     promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_imessage_reply_context_total", Help: "iMessage reply context lookups by result."}, []string{"result"}),
		errorsTotal:              promauto.NewCounterVec(prometheus.CounterOpts{Name: "oswald_errors_total", Help: "Normalized errors by component and type."}, []string{"component", "type"}),
	}
}

// Handler returns the Prometheus scrape handler.
func (m *Metrics) Handler() http.Handler { return promhttp.Handler() }

func (m *Metrics) ObserveGatewayReceived(gateway, kind string) {
	if m == nil {
		return
	}
	if kind == "" {
		kind = "unknown"
	}
	m.gatewayReceivedTotal.WithLabelValues(gateway, kind).Inc()
}

func (m *Metrics) ObserveGatewayIgnored(gateway, reason string) {
	if m == nil {
		return
	}
	m.gatewayIgnoredTotal.WithLabelValues(gateway, reason).Inc()
}

func (m *Metrics) ObserveGatewaySent(gateway, result string) {
	if m == nil {
		return
	}
	m.gatewaySentTotal.WithLabelValues(gateway, result).Inc()
}

func (m *Metrics) ObserveGatewaySendDuration(gateway, result string, duration time.Duration) {
	if m == nil {
		return
	}
	m.gatewaySendDuration.WithLabelValues(gateway, result).Observe(duration.Seconds())
}

func (m *Metrics) ObserveGatewaySendFailure(gateway, reason string) {
	if m == nil {
		return
	}
	m.gatewaySendFailures.WithLabelValues(gateway, reason).Inc()
}

func (m *Metrics) ObserveRequestShape(gateway string, inputChars int, imageCount int, imageBytes int) {
	if m == nil {
		return
	}
	m.requestInputChars.WithLabelValues(gateway).Observe(float64(inputChars))
	m.requestImages.WithLabelValues(gateway).Observe(float64(imageCount))
	m.requestImageBytes.WithLabelValues(gateway).Observe(float64(imageBytes))
}

func (m *Metrics) ObserveRequestOutput(gateway string, outputChars int) {
	if m == nil {
		return
	}
	m.requestOutputChars.WithLabelValues(gateway).Observe(float64(outputChars))
}

func (m *Metrics) ObserveBrokerEnqueue(queueDepth int) {
	if m == nil {
		return
	}
	m.brokerQueueDepth.Set(float64(queueDepth))
}

func (m *Metrics) ObserveBrokerDequeue(gateway string, wait time.Duration, queueDepth int) {
	if m == nil {
		return
	}
	m.brokerQueueWait.WithLabelValues(gateway).Observe(wait.Seconds())
	m.brokerQueueDepth.Set(float64(queueDepth))
}

func (m *Metrics) ObserveBrokerRejection(gateway, reason string, queueDepth int) {
	if m == nil {
		return
	}
	m.brokerRejections.WithLabelValues(gateway, reason).Inc()
	m.brokerQueueDepth.Set(float64(queueDepth))
}

func (m *Metrics) IncActiveRequest(gateway string) {
	if m == nil {
		return
	}
	m.activeRequests.WithLabelValues(gateway).Inc()
	m.brokerWorkersActive.Inc()
}

func (m *Metrics) DecActiveRequest(gateway string) {
	if m == nil {
		return
	}
	m.activeRequests.WithLabelValues(gateway).Dec()
	m.brokerWorkersActive.Dec()
}

func (m *Metrics) ObserveRequest(gateway, userID, result string, duration time.Duration) {
	if m == nil {
		return
	}
	m.requestsTotal.WithLabelValues(gateway, result).Inc()
	m.requestDuration.WithLabelValues(gateway, result).Observe(duration.Seconds())
	if userID != "" {
		m.userRequestsTotal.WithLabelValues(gateway, userID).Inc()
	}
}

func (m *Metrics) ObserveBrokerProcessing(gateway, result string, duration time.Duration) {
	if m == nil {
		return
	}
	m.brokerProcessingDuration.WithLabelValues(gateway, result).Observe(duration.Seconds())
}

func (m *Metrics) ObserveAgentRequest(gateway string, stream bool, iterations int, toolCalls int, finalEmpty bool, toolBudgetStopped bool) {
	if m == nil {
		return
	}
	m.agentStreamingRequests.WithLabelValues(gateway, strconv.FormatBool(stream)).Inc()
	m.agentIterations.WithLabelValues(gateway).Observe(float64(iterations))
	m.agentToolCallsPerRequest.WithLabelValues(gateway).Observe(float64(toolCalls))
	if finalEmpty {
		m.agentFinalEmptyTotal.WithLabelValues(gateway).Inc()
	}
	if toolBudgetStopped {
		m.agentToolBudgetStops.Inc()
	}
}

func (m *Metrics) ObserveMemoryLoad(gateway string, turnsLoaded int) {
	if m == nil {
		return
	}
	m.memoryTurnsLoaded.WithLabelValues(gateway).Observe(float64(turnsLoaded))
}

func (m *Metrics) ObserveMemoryPersist(gateway string, turnsPersisted int) {
	if m == nil {
		return
	}
	m.memoryTurnsPersisted.WithLabelValues(gateway).Observe(float64(turnsPersisted))
}

func (m *Metrics) ObserveMemoryCompaction(removedPairs int) {
	if m == nil {
		return
	}
	m.memoryCompactions.Inc()
	m.memoryCompactedPairs.Observe(float64(removedPairs))
}

func (m *Metrics) ObservePromptBudget(model string, estimatedTokens int, actualTokens int, promptBudget int, stillOverBudget bool) {
	if m == nil {
		return
	}
	if estimatedTokens > 0 {
		m.memoryPromptEstimate.WithLabelValues(model).Observe(float64(estimatedTokens))
		if promptBudget > 0 {
			m.memoryBudgetUtilization.WithLabelValues(model, "estimate").Observe(float64(estimatedTokens) / float64(promptBudget))
		}
	}
	if actualTokens > 0 {
		m.memoryActualPrompt.WithLabelValues(model).Observe(float64(actualTokens))
		if promptBudget > 0 {
			m.memoryBudgetUtilization.WithLabelValues(model, "actual").Observe(float64(actualTokens) / float64(promptBudget))
		}
	}
	if stillOverBudget {
		m.memoryOverBudgetTotal.WithLabelValues(model).Inc()
	}
}

func (m *Metrics) SetMemoryStoreState(sessionCount int, turnCount int) {
	if m == nil {
		return
	}
	m.memorySessionsCurrent.Set(float64(sessionCount))
	m.memoryTurnsCurrent.Set(float64(turnCount))
}

func (m *Metrics) ObserveLLMCall(model string, stream bool, result string, duration time.Duration, promptTokens, evalTokens int, evalDurationNS int64) {
	if m == nil {
		return
	}
	streamLabel := strconv.FormatBool(stream)
	m.llmCallsTotal.WithLabelValues(model, streamLabel, result).Inc()
	m.llmCallDuration.WithLabelValues(model, streamLabel, result).Observe(duration.Seconds())
	if promptTokens > 0 {
		m.llmPromptTokens.WithLabelValues(model).Observe(float64(promptTokens))
	}
	if evalTokens > 0 {
		m.llmEvalTokens.WithLabelValues(model).Observe(float64(evalTokens))
	}
	if evalTokens > 0 && evalDurationNS > 0 {
		tps := float64(evalTokens) / (float64(evalDurationNS) / float64(time.Second))
		m.llmTokensPerSecond.WithLabelValues(model).Observe(tps)
	}
}

func (m *Metrics) ObserveLLMResponseMetrics(model string, ttft time.Duration, loadDurationNS int64, promptEvalDurationNS int64, evalDurationNS int64, doneReason string, hadToolCalls bool) {
	if m == nil {
		return
	}
	if ttft > 0 {
		m.llmTimeToFirstToken.WithLabelValues(model).Observe(ttft.Seconds())
	}
	if loadDurationNS > 0 {
		m.llmLoadDuration.WithLabelValues(model).Observe(float64(loadDurationNS) / float64(time.Second))
	}
	if promptEvalDurationNS > 0 {
		m.llmPromptEvalDuration.WithLabelValues(model).Observe(float64(promptEvalDurationNS) / float64(time.Second))
	}
	if evalDurationNS > 0 {
		m.llmEvalDuration.WithLabelValues(model).Observe(float64(evalDurationNS) / float64(time.Second))
	}
	if doneReason != "" {
		m.llmDoneReasonTotal.WithLabelValues(model, doneReason).Inc()
	}
	if hadToolCalls {
		m.llmToolCallMessagesTotal.WithLabelValues(model).Inc()
	}
}

func (m *Metrics) ObserveTool(tool, result, errorType string, duration time.Duration, argChars int, resultChars int) {
	if m == nil {
		return
	}
	m.toolsTotal.WithLabelValues(tool, result).Inc()
	m.toolDuration.WithLabelValues(tool, result).Observe(duration.Seconds())
	m.toolArgsSize.WithLabelValues(tool).Observe(float64(argChars))
	m.toolResultSize.WithLabelValues(tool).Observe(float64(resultChars))
	if errorType != "" {
		m.toolFailuresTotal.WithLabelValues(tool, errorType).Inc()
	}
}

func (m *Metrics) ObserveUnsupportedAttachment(gateway, reason string) {
	if m == nil {
		return
	}
	m.unsupportedAttachments.WithLabelValues(gateway, reason).Inc()
}

func (m *Metrics) ObserveAttachmentDeclaredMIME(gateway, mime string) {
	if m == nil {
		return
	}
	if mime == "" {
		mime = "unknown"
	}
	m.attachmentDeclaredMIME.WithLabelValues(gateway, mime).Inc()
}

func (m *Metrics) ObserveAttachmentDetectedMIME(gateway, mime string) {
	if m == nil {
		return
	}
	if mime == "" {
		mime = "unknown"
	}
	m.attachmentDetectedMIME.WithLabelValues(gateway, mime).Inc()
}

func (m *Metrics) IncWebsocketConnections() {
	if m == nil {
		return
	}
	m.websocketConnections.Inc()
}

func (m *Metrics) DecWebsocketConnections() {
	if m == nil {
		return
	}
	m.websocketConnections.Dec()
}

func (m *Metrics) ObserveWebsocketStreamChunk(chunkType string) {
	if m == nil {
		return
	}
	m.websocketStreamChunks.WithLabelValues(chunkType).Inc()
}

func (m *Metrics) ObserveWebsocketWriteFailure() {
	if m == nil {
		return
	}
	m.websocketWriteFailures.Inc()
}

func (m *Metrics) ObserveDiscordTyping(result string) {
	if m == nil {
		return
	}
	m.discordTypingRequests.WithLabelValues(result).Inc()
}

func (m *Metrics) ObserveDiscordReplyContext(result string) {
	if m == nil {
		return
	}
	m.discordReplyContext.WithLabelValues(result).Inc()
}

func (m *Metrics) ObserveDiscordSplitChunks(count int) {
	if m == nil {
		return
	}
	m.discordSplitChunks.Observe(float64(count))
}

func (m *Metrics) ObserveIMessageWebhook(eventType, result string) {
	if m == nil {
		return
	}
	m.imessageWebhooks.WithLabelValues(eventType, result).Inc()
}

func (m *Metrics) ObserveIMessageTyping(result string) {
	if m == nil {
		return
	}
	m.imessageTypingRequests.WithLabelValues(result).Inc()
}

func (m *Metrics) ObserveIMessageContactLookup(result string) {
	if m == nil {
		return
	}
	m.imessageContactLookups.WithLabelValues(result).Inc()
}

func (m *Metrics) ObserveIMessageReplyContext(result string) {
	if m == nil {
		return
	}
	m.imessageReplyContext.WithLabelValues(result).Inc()
}

func (m *Metrics) ObserveError(component, errorType string) {
	if m == nil {
		return
	}
	m.errorsTotal.WithLabelValues(component, errorType).Inc()
}
