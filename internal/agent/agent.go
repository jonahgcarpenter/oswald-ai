package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/debug"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/soulmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/usermemory"
)

const compactedHistoryUserPrompt = "Here is the compacted history from previous messages."

// StreamChunkType identifies the kind of content in a StreamChunk.
type StreamChunkType string

const (
	// ChunkThinking carries tokens from the model's internal reasoning phase.
	ChunkThinking StreamChunkType = "thinking"

	// ChunkContent carries tokens from the model's visible response.
	ChunkContent StreamChunkType = "content"

	// ChunkStatus carries status messages injected by the agent (e.g. "[Calling: web_search]").
	ChunkStatus StreamChunkType = "status"
)

// StreamChunk is a single typed token event streamed to gateways during Process().
// Gateways receive thinking tokens, content tokens, and agent status messages via this type.
type StreamChunk struct {
	Type StreamChunkType `json:"type"`
	Text string          `json:"text"`
}

// ModelMetrics holds performance data from a single LLM call.
type ModelMetrics struct {
	Model              string  `json:"model"`
	TotalDuration      int64   `json:"total_duration_ms"`
	LoadDuration       int64   `json:"load_duration_ms"`
	PromptEvalDuration int64   `json:"prompt_eval_duration_ms"`
	EvalDuration       int64   `json:"eval_duration_ms"`
	TokensPerSecond    float64 `json:"tokens_per_second"`
}

// AgentResponse is the final payload returned to the gateway after processing.
type AgentResponse struct {
	Model    string        `json:"model"`
	Response string        `json:"response,omitempty"`
	Thinking string        `json:"thinking,omitempty"` // reasoning tokens emitted before the response
	Error    string        `json:"error,omitempty"`
	Metrics  *ModelMetrics `json:"metrics,omitempty"`
}

// Agent handles LLM orchestration: a single agentic loop where the model
// calls tools from the registry and generates the final response.
type Agent struct {
	chatClient            ollama.Chatter
	registry              *tools.Registry
	memory                *memory.Store
	budget                memory.ContextBudget
	model                 string
	soul                  *soulmemory.Store
	userMemory            *usermemory.Store
	summarizer            *OllamaSummarizer
	agentTracePath        string // directory for per-request agent trace dumps; empty disables
	maxToolFailureRetries int
	log                   *config.Logger
}

// NewAgent initializes the Agent with an Ollama chat client, tool registry, model name,
// soul store, conversation memory store, tool failure retry budget, and logger.
func NewAgent(
	chatClient ollama.Chatter,
	registry *tools.Registry,
	model string,
	soul *soulmemory.Store,
	userMemory *usermemory.Store,
	budget memory.ContextBudget,
	maxToolFailureRetries int,
	memoryStore *memory.Store,
	agentTracePath string,
	log *config.Logger,
) *Agent {
	return &Agent{
		chatClient:            chatClient,
		registry:              registry,
		memory:                memoryStore,
		budget:                budget,
		model:                 model,
		soul:                  soul,
		userMemory:            userMemory,
		summarizer:            NewOllamaSummarizer(chatClient, model, log),
		agentTracePath:        agentTracePath,
		maxToolFailureRetries: maxToolFailureRetries,
		log:                   log,
	}
}

// truncate returns s shortened to at most max runes, appending "..." if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

// mapMetrics converts an *ollama.ChatResponse into a *ModelMetrics summary for reporting.
// Returns nil if the response is missing or has no evaluation duration (partial failure).
// Converts nanosecond timings to milliseconds and calculates tokens/second throughput.
func mapMetrics(resp *ollama.ChatResponse) *ModelMetrics {
	if resp == nil || resp.EvalDuration <= 0 {
		return nil
	}
	tps := float64(resp.EvalCount) / (float64(resp.EvalDuration) / 1e9)
	return &ModelMetrics{
		Model:              resp.Model,
		TotalDuration:      resp.TotalDuration / 1e6,
		LoadDuration:       resp.LoadDuration / 1e6,
		PromptEvalDuration: resp.PromptEvalDuration / 1e6,
		EvalDuration:       resp.EvalDuration / 1e6,
		TokensPerSecond:    tps,
	}
}

func makeCompactedTurn(summary string, now time.Time) memory.Turn {
	return memory.Turn{
		CreatedAt: now.UTC(),
		User: ollama.ChatMessage{
			Role:    "user",
			Content: compactedHistoryUserPrompt,
		},
		Assistant: ollama.ChatMessage{
			Role:    "assistant",
			Content: summary,
		},
	}
}

func (a *Agent) compactTurnsToFit(ctx context.Context, log *config.Logger, systemPrompt string, turns []memory.Turn, userPrompt string, userImages []ollama.InputImage) ([]memory.Turn, memory.PromptPruneResult, bool) {
	tools := a.registry.OllamaTools()
	currentTurns := append([]memory.Turn(nil), turns...)
	persisted := false
	result := memory.PromptPruneResult{
		EstimatedBefore: memory.EstimatePromptTokens(systemPrompt, memory.FlattenTurns(currentTurns), userPrompt, len(userImages), tools),
	}
	lastEstimate := result.EstimatedBefore

	for len(currentTurns) > 0 {
		history := memory.FlattenTurns(currentTurns)
		estimate := memory.EstimatePromptTokens(systemPrompt, history, userPrompt, len(userImages), tools)
		lastEstimate = estimate
		if estimate <= a.budget.PromptBudget() {
			result.EstimatedAfter = estimate
			return currentTurns, result, persisted
		}

		compacted := false
		for compactCount := 1; compactCount <= len(currentTurns); compactCount++ {
			summary, err := a.summarizer.Summarize(ctx, currentTurns[:compactCount])
			if err != nil {
				log.Warn("agent.context.compaction_failed", "failed to compact context",
					config.F("turn_pair_count", compactCount),
					config.ErrorField(err),
				)
				continue
			}

			replacement := makeCompactedTurn(summary, time.Now())
			candidateTurns := make([]memory.Turn, 0, 1+len(currentTurns)-compactCount)
			candidateTurns = append(candidateTurns, replacement)
			candidateTurns = append(candidateTurns, currentTurns[compactCount:]...)

			candidateEstimate := memory.EstimatePromptTokens(systemPrompt, memory.FlattenTurns(candidateTurns), userPrompt, len(userImages), tools)
			if candidateEstimate >= estimate && compactCount < len(currentTurns) {
				continue
			}

			currentTurns = candidateTurns
			persisted = true
			result.RemovedPairs += compactCount
			compacted = true
			break
		}

		if !compacted {
			break
		}
	}

	result.EstimatedAfter = lastEstimate
	return currentTurns, result, persisted
}

// Process handles the end-to-end agentic pipeline in a single loop.
// The model receives all registered tools and may call them zero or more times
// before generating its final response. Thinking tokens, content tokens, and
// agent status messages are streamed via streamCallback if provided.
//
// sessionKey identifies the conversation session for memory retrieval and
// persistence. Passing an empty sessionKey disables memory for this request
// (stateless one-shot behaviour).
//
// senderID is the stable internal user identifier for the current request. It is
// injected into the request context so that tools such as persistent_memory can
// identify the user without needing the session key. An empty senderID disables
// user-scoped tool behaviour.
//
// displayName is the human-readable name supplied by the active gateway.
// The agent prefers the persistent-memory intro and canonical account links for
// speaker identity, but keeps this argument for request logging.
//
// Tool execution errors are handled gracefully — failures inject an error tool
// response so the model can decide how to proceed. Provider errors are captured
// into AgentResponse.Error rather than returned as Go errors.
func (a *Agent) Process(requestID string, gateway string, sessionKey string, senderID string, displayName string, userPrompt string, userImages []ollama.InputImage, streamCallback func(chunk StreamChunk)) (*AgentResponse, error) {
	startedAt := time.Now()
	reqLog := a.log.Agent("agent", requestID, sessionKey, senderID, gateway, a.model)
	reqLog.Debug("agent.request.start", "agent request started",
		config.F("display_name", displayName),
		config.F("prompt_chars", len(userPrompt)),
		config.F("image_count", len(userImages)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Inject the sender ID into the context so tool handlers can identify
	// which user this request belongs to without coupling to the session key.
	ctx = toolctx.WithSenderID(ctx, senderID)
	ctx = toolctx.WithMetadata(ctx, toolctx.Metadata{
		RequestID: requestID,
		SessionID: sessionKey,
		SenderID:  senderID,
		Gateway:   gateway,
		Model:     a.model,
	})

	// Read the soul file fresh on every request so that any edits the agent
	// made via the soul_memory tool take effect immediately.
	soulContent, soulErr := a.soul.Read()
	if soulErr != nil {
		reqLog.Warn("agent.soul.read_failed", "failed to read soul file", config.ErrorField(soulErr))
	}

	// Build the dynamic system prompt: soul + timestamp + speaker identity.
	// User memory is not injected automatically — the model retrieves it via
	// the persistent_memory tool when needed.
	var promptParts []string
	promptParts = append(promptParts, soulContent)

	if speakerLine := a.currentSpeakerLine(reqLog, senderID); speakerLine != "" {
		promptParts = append(promptParts, "# Current Speaker\n"+speakerLine)
	}
	promptParts = append(promptParts, a.userMemoryPromptSections(reqLog, senderID)...)
	promptParts = append(promptParts, "# Current Date and Time\n"+time.Now().UTC().Format(time.RFC1123))

	dynamicSystemPrompt := strings.Join(promptParts, "\n\n")

	// Retrieve retained conversation turns for this session. TTL and max-turn
	// pruning are applied destructively inside the store before history is used.
	turns := a.memory.Turns(sessionKey)

	// If the estimated prompt would exceed the active model budget, destructively
	// compact the oldest retained turns into a synthetic turn pair that remains
	// in ordinary conversation history.
	compactedTurns, prune, compacted := a.compactTurnsToFit(ctx, reqLog, dynamicSystemPrompt, turns, userPrompt, userImages)
	if compacted {
		a.memory.ReplaceTurns(sessionKey, compactedTurns)
		turns = compactedTurns
	}
	trimmedHistory := memory.FlattenTurns(turns)

	messageImages := make([]string, 0, len(userImages))
	for _, image := range userImages {
		messageImages = append(messageImages, image.Data)
	}

	messages := make([]ollama.ChatMessage, 0, 2+len(trimmedHistory))
	messages = append(messages, ollama.ChatMessage{Role: "system", Content: dynamicSystemPrompt})
	messages = append(messages, trimmedHistory...)
	messages = append(messages, ollama.ChatMessage{Role: "user", Content: userPrompt, Images: messageImages})

	if len(trimmedHistory) > 0 {
		reqLog.Debug("agent.memory.loaded", "loaded retained memory",
			config.F("historical_message_count", len(trimmedHistory)),
			config.F("turn_pair_count", len(turns)),
		)
	}
	if prune.RemovedPairs > 0 {
		reqLog.Debug("agent.context.compacted", "compacted retained context",
			config.F("removed_pair_count", prune.RemovedPairs),
			config.F("prompt_budget", a.budget.PromptBudget()),
			config.F("estimated_before", prune.EstimatedBefore),
			config.F("estimated_after", prune.EstimatedAfter),
		)
	}
	if prune.EstimatedAfter > a.budget.PromptBudget() {
		reqLog.Warn("agent.context.over_budget", "prompt still exceeds budget after compaction",
			config.F("estimated_after", prune.EstimatedAfter),
			config.F("prompt_budget", a.budget.PromptBudget()),
		)
	}

	req := ollama.ChatRequest{
		Model:  a.model,
		Tools:  a.registry.OllamaTools(),
		Stream: streamCallback != nil,
	}

	// Track accumulated thinking and content across all iterations.
	// The model may emit thinking tokens in any iteration; content tokens only
	// appear in the final response turn (when no tool calls are made).
	var accumulatedThinking strings.Builder
	var accumulatedContent strings.Builder
	toolExecutions := make([]debug.ToolExecutionTrace, 0)

	// consecutiveToolFailures tracks back-to-back tool execution failures for
	// this request. A successful tool call resets the counter.
	consecutiveToolFailures := 0

	// toolAnnotations collects brief notes about tools used this request.
	// These are appended to the stored assistant message so future turns
	// show what tools were called without ballooning history size.
	var toolAnnotations []string

	// Build the streaming callback that routes thinking vs content chunks.
	// Tool-call iterations are streamed too — the model may reason aloud before
	// deciding to call a tool. The stream pauses naturally while tools execute.
	var chatCallback func(ollama.ChatMessage)
	if streamCallback != nil {
		chatCallback = func(chunk ollama.ChatMessage) {
			if chunk.Thinking != "" {
				accumulatedThinking.WriteString(chunk.Thinking)
				streamCallback(StreamChunk{Type: ChunkThinking, Text: chunk.Thinking})
			}
			if chunk.Content != "" {
				accumulatedContent.WriteString(chunk.Content)
				streamCallback(StreamChunk{Type: ChunkContent, Text: chunk.Content})
			}
		}
	}

	var lastResp *ollama.ChatResponse
	toolFailureBudgetExhausted := false

	// Agentic loop: the model runs, may call tools, receives results, then runs again.
	// The loop exits when the model stops issuing tool calls, the request context
	// expires, or consecutive tool execution failures exhaust the retry budget.
	for iteration := 1; ; iteration++ {
		// Reset the content accumulator each iteration — we only keep the final
		// response turn's content. Thinking is accumulated across all iterations.
		accumulatedContent.Reset()

		req.Messages = messages
		reqLog.Debug("agent.model.call", "calling model",
			config.F("iteration", iteration),
			config.F("is_streaming", req.Stream),
			config.F("tool_count", len(req.Tools)),
		)

		resp, err := a.chatClient.Chat(ctx, req, chatCallback)
		if err != nil {
			reqLog.Error("agent.model.error", "model call failed", config.F("iteration", iteration), config.ErrorField(err))
			return &AgentResponse{
				Model:    a.model,
				Response: "Something broke, Try again or help fragsap buy a new GPU to fix these issues.",
				Error:    fmt.Sprintf("Model failed: %v", err),
			}, nil
		}

		lastResp = resp
		if iteration == 1 && resp.PromptEvalCount > 0 {
			reqLog.Debug("agent.context.estimated_vs_actual", "compared estimated and actual prompt tokens",
				config.F("estimated_after", prune.EstimatedAfter),
				config.F("actual_prompt_tokens", resp.PromptEvalCount),
			)
		}

		reqLog.Debug("agent.loop.iteration", "completed agent loop iteration",
			config.F("iteration", iteration),
			config.F("tool_call_count", len(resp.Message.ToolCalls)),
			config.F("thinking_chars", len(resp.Message.Thinking)),
			config.F("content_chars", len(resp.Message.Content)),
			config.F("failure_streak", consecutiveToolFailures),
		)

		// No tool calls — the model is done. Exit the loop.
		if len(resp.Message.ToolCalls) == 0 {
			reqLog.Debug("agent.loop.complete", "agent loop completed", config.F("iteration_count", iteration), config.F("status", "ok"))
			break
		}

		// Append the assistant turn (including its tool calls) to the conversation.
		messages = append(messages, resp.Message)

		// Execute each tool call and inject the results as tool response messages.
		// NOTE: Most models only emit one tool call at a time, but we handle
		// multiple to be safe.
		for _, tc := range resp.Message.ToolCalls {
			toolName := tc.Function.Name
			toolStartedAt := time.Now()
			traceEntry := debug.ToolExecutionTrace{
				Iteration: iteration,
				Name:      toolName,
				Arguments: tc.Function.Arguments,
			}

			// Emit a status chunk so the gateway knows a tool is executing.
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkStatus, Text: fmt.Sprintf("[Calling: %s]", toolName)})
			}
			reqLog.Debug("agent.tool.start", "starting tool execution",
				config.F("iteration", iteration),
				config.F("tool_name", toolName),
			)

			var toolContent string

			result, execErr := a.registry.Execute(ctx, toolName, tc.Function.Arguments)
			if execErr != nil {
				// Fail gracefully: inject the error so the model can recover.
				consecutiveToolFailures++
				reqLog.Warn("agent.tool.failure", "tool execution failed",
					config.F("iteration", iteration),
					config.F("tool_name", toolName),
					config.F("failure_streak", consecutiveToolFailures),
					config.F("max_failures", a.maxToolFailureRetries),
					config.F("duration_ms", time.Since(toolStartedAt).Milliseconds()),
					config.F("status", "error"),
					config.ErrorField(execErr),
				)
				toolContent = fmt.Sprintf("Error: %v", execErr)
				traceEntry.Error = execErr.Error()
			} else {
				consecutiveToolFailures = 0
				toolContent = result
				reqLog.Debug("agent.tool.success", "tool execution succeeded",
					config.F("iteration", iteration),
					config.F("tool_name", toolName),
					config.F("duration_ms", time.Since(toolStartedAt).Milliseconds()),
					config.F("status", "ok"),
				)
				// Record a brief annotation for history storage.
				toolAnnotations = append(toolAnnotations, toolName)
			}
			traceEntry.Result = toolContent
			toolExecutions = append(toolExecutions, traceEntry)

			messages = append(messages, ollama.ChatMessage{
				Role:     "tool",
				ToolName: toolName,
				Content:  toolContent,
			})

			if a.maxToolFailureRetries > 0 && consecutiveToolFailures >= a.maxToolFailureRetries {
				reqLog.Warn("agent.tool_budget.exhausted", "tool failure budget exhausted", config.F("failure_streak", consecutiveToolFailures), config.F("max_failures", a.maxToolFailureRetries), config.F("status", "degraded"))
				toolFailureBudgetExhausted = true
				break
			}
		}

		if a.maxToolFailureRetries > 0 && consecutiveToolFailures >= a.maxToolFailureRetries {
			break
		}
	}

	if toolFailureBudgetExhausted {
		accumulatedContent.Reset()
		finalReq := req
		finalReq.Messages = messages
		finalReq.Tools = nil

		resp, err := a.chatClient.Chat(ctx, finalReq, chatCallback)
		if err != nil {
			reqLog.Error("agent.model.error", "model finish failed after tool failures", config.ErrorField(err))
			return &AgentResponse{
				Model:    a.model,
				Response: "Something broke, Try again or help fragsap buy a new GPU to fix these issues.",
				Error:    fmt.Sprintf("Model failed: %v", err),
			}, nil
		}

		lastResp = resp
		reqLog.Debug("agent.loop.complete", "completed agent loop after disabling tools",
			config.F("iteration_count", len(toolExecutions)+1),
			config.F("failure_streak", consecutiveToolFailures),
			config.F("status", "degraded"),
		)
	}

	// Extract the final response content. The Ollama client already handles
	// thinking-to-content promotion for non-streaming calls.
	// For streaming, we tracked content separately via the callback above.
	finalContent := accumulatedContent.String()
	if finalContent == "" && lastResp != nil {
		finalContent = lastResp.Message.Content
	}

	finalThinking := accumulatedThinking.String()
	if finalThinking == "" && lastResp != nil {
		finalThinking = lastResp.Message.Thinking
	}
	if lastResp != nil {
		messages = append(messages, lastResp.Message)
	}

	if a.agentTracePath != "" {
		if dumpErr := debug.DumpAgentTrace(debug.AgentTrace{
			Dir:                        a.agentTracePath,
			SessionKey:                 sessionKey,
			Model:                      a.model,
			Messages:                   messages,
			Tools:                      a.registry.OllamaTools(),
			ContextWindow:              a.budget.ContextWindow,
			PromptBudget:               a.budget.PromptBudget(),
			EstimatedBefore:            prune.EstimatedBefore,
			EstimatedAfter:             prune.EstimatedAfter,
			RemovedPairs:               prune.RemovedPairs,
			RequestImages:              userImages,
			ToolExecutions:             toolExecutions,
			FinalResponse:              finalContent,
			FinalThinking:              finalThinking,
			FinalModelResponse:         lastResp,
			ToolFailureBudgetExhausted: toolFailureBudgetExhausted,
		}); dumpErr != nil {
			reqLog.Warn("agent.trace.write_failed", "failed to write agent trace", config.ErrorField(dumpErr))
		} else {
			reqLog.Debug("agent.trace.written", "wrote agent trace", config.F("path", a.agentTracePath))
		}
	}

	// Persist the user prompt and the assistant's final response to memory.
	// Tool-call intermediaries are intentionally excluded to keep history lean —
	// only the conversational exchange (not the internal reasoning steps) is retained.
	if finalContent != "" {
		userMemoryContent := userPrompt
		if len(userImages) > 0 {
			userMemoryContent = strings.TrimSpace(userMemoryContent + fmt.Sprintf("\n\n[Attached %d image(s)]", len(userImages)))
		}
		a.memory.AppendTurn(
			sessionKey,
			ollama.ChatMessage{Role: "user", Content: userMemoryContent},
			ollama.ChatMessage{Role: "assistant", Content: finalContent},
		)
	}

	reqLog.Debug("agent.response.complete", "completed agent response",
		config.F("iteration_count", len(toolExecutions)+1),
		config.F("response_chars", len(finalContent)),
		config.F("thinking_chars", len(finalThinking)),
		config.F("tool_call_count", len(toolExecutions)),
		config.F("duration_ms", time.Since(startedAt).Milliseconds()),
		config.F("tool_failure_budget_exhausted", toolFailureBudgetExhausted),
		config.F("status", "ok"),
	)

	return &AgentResponse{
		Model:    a.model,
		Response: finalContent,
		Thinking: finalThinking,
		Metrics:  mapMetrics(lastResp),
	}, nil
}

func (a *Agent) currentSpeakerLine(log *config.Logger, senderID string) string {
	if senderID == "" {
		return ""
	}

	if a.userMemory != nil {
		intro, err := a.userMemory.ReadIntro(senderID)
		if err != nil {
			log.Warn("agent.user_memory_intro.read_failed", "failed to read user memory intro", config.F("user_id", senderID), config.ErrorField(err))
		} else if strings.TrimSpace(intro) != "" {
			return strings.TrimSpace(intro)
		}
	}

	return ""
}

func (a *Agent) userMemoryPromptSections(log *config.Logger, senderID string) []string {
	if senderID == "" || a.userMemory == nil {
		return nil
	}

	sections := make([]string, 0, 2)
	if identity := a.userMemoryPromptSection(log, senderID, "identity", "## User Identity Memory"); identity != "" {
		sections = append(sections, identity)
	}
	if systemRules := a.userMemoryPromptSection(log, senderID, "system_rules", "## User System Rules"); systemRules != "" {
		sections = append(sections, systemRules)
	}
	return sections
}

func (a *Agent) userMemoryPromptSection(log *config.Logger, senderID, category, heading string) string {
	content, err := a.userMemory.ReadCategory(senderID, category)
	if err != nil {
		log.Warn("agent.user_memory_category.read_failed", "failed to read user memory category", config.F("category", category), config.F("user_id", senderID), config.ErrorField(err))
		return ""
	}
	body := stripMarkdownHeading(content)
	if body == "" {
		return ""
	}
	return heading + "\n" + body
}

func stripMarkdownHeading(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return ""
	}
	if strings.HasPrefix(lines[0], "## ") {
		return strings.TrimSpace(strings.Join(lines[1:], "\n"))
	}
	return content
}
