package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	toolruntime "github.com/jonahgcarpenter/oswald-ai/internal/tools/runtime"
)

const (
	memoryRecentTurns        = 4
	memoryRetrievalLimit     = 12
	memoryContextBudgetRatio = 0.15
	sessionTurnTTL           = 24 * time.Hour
)

// StreamChunkType identifies the kind of content in a StreamChunk.
type StreamChunkType string

const (
	// ChunkThinking carries tokens from the model's internal reasoning phase.
	ChunkThinking StreamChunkType = "thinking"

	// ChunkContent carries tokens from the model's visible response.
	ChunkContent StreamChunkType = "content"

	// ChunkStatus carries status messages injected by the agent (e.g. "[Calling: web.search]").
	ChunkStatus StreamChunkType = "status"

	// ChunkToolCall carries structured tool invocation data for frontend timelines.
	ChunkToolCall StreamChunkType = "tool_call"

	// ChunkToolResult carries structured tool result data for frontend timelines.
	ChunkToolResult StreamChunkType = "tool_result"
)

// ToolStreamSearchResult is a UI-safe search result emitted for web.search tools.
type ToolStreamSearchResult struct {
	Title   string `json:"title,omitempty"`
	URL     string `json:"url,omitempty"`
	Content string `json:"content,omitempty"`
}

// ToolStreamSearchPayload contains structured web.search details for streaming UIs.
type ToolStreamSearchPayload struct {
	Query   string                   `json:"query,omitempty"`
	Results []ToolStreamSearchResult `json:"results,omitempty"`
}

// ToolStreamPayload contains structured tool data for frontend rendering.
type ToolStreamPayload struct {
	Name       string                   `json:"name"`
	Arguments  map[string]interface{}   `json:"arguments,omitempty"`
	ResultText string                   `json:"result_text,omitempty"`
	DurationMS int64                    `json:"duration_ms,omitempty"`
	IsError    bool                     `json:"is_error,omitempty"`
	WebSearch  *ToolStreamSearchPayload `json:"web.search,omitempty"`
	Memory     *ToolStreamMemoryPayload `json:"memory,omitempty"`
	Soul       *ToolStreamSoulPayload   `json:"soul,omitempty"`
}

// ToolStreamMemoryPayload contains structured memory tool details.
type ToolStreamMemoryPayload struct {
	Action    string                    `json:"action,omitempty"`
	Category  string                    `json:"category,omitempty"`
	Statement string                    `json:"statement,omitempty"`
	Evidence  string                    `json:"evidence,omitempty"`
	Content   *usermemory.ParsedContent `json:"content,omitempty"`
}

// ToolStreamSoulPayload contains structured soul tool details.
type ToolStreamSoulPayload struct {
	Action    string `json:"action,omitempty"`
	Operation string `json:"operation,omitempty"`
	Target    string `json:"target,omitempty"`
	Anchor    string `json:"anchor,omitempty"`
	Position  string `json:"position,omitempty"`
	Content   string `json:"content,omitempty"`
}

// StreamChunk is a single typed token event streamed to gateways during Process().
// Gateways receive thinking tokens, content tokens, and agent status messages via this type.
type StreamChunk struct {
	Type StreamChunkType    `json:"type"`
	Text string             `json:"text,omitempty"`
	Tool *ToolStreamPayload `json:"tool,omitempty"`
}

func toolStreamPayload(toolName string, args map[string]interface{}, result string, duration time.Duration, isError bool) *ToolStreamPayload {
	payload := &ToolStreamPayload{
		Name:       toolName,
		Arguments:  args,
		ResultText: result,
		DurationMS: duration.Milliseconds(),
		IsError:    isError,
	}

	if toolName != "web.search" {
		switch toolName {
		case "memory.save", "memory.search", "memory.list", "memory.forget":
			payload.Memory = memoryStreamPayload(toolName, args, result, isError)
		case "soul.read", "soul.patch":
			payload.Soul = soulStreamPayload(toolName, args, result, isError)
		}
		return payload
	}

	searchPayload := &ToolStreamSearchPayload{}
	if query, ok := args["query"].(string); ok {
		searchPayload.Query = strings.TrimSpace(query)
	}
	results := websearch.ParseFormattedResults(result)
	if len(results) > 0 {
		searchPayload.Results = make([]ToolStreamSearchResult, 0, len(results))
		for _, r := range results {
			searchPayload.Results = append(searchPayload.Results, ToolStreamSearchResult{
				Title:   r.Title,
				URL:     r.URL,
				Content: r.Content,
			})
		}
	}
	payload.WebSearch = searchPayload
	return payload
}

func memoryStreamPayload(toolName string, args map[string]interface{}, result string, isError bool) *ToolStreamMemoryPayload {
	payload := &ToolStreamMemoryPayload{}
	payload.Action = memoryToolAction(toolName)
	if category, ok := args["category"].(string); ok {
		payload.Category = strings.TrimSpace(strings.ToLower(category))
	}
	if statement, ok := args["statement"].(string); ok {
		payload.Statement = strings.TrimSpace(statement)
	}
	if evidence, ok := args["evidence"].(string); ok {
		payload.Evidence = strings.TrimSpace(evidence)
	}
	if isError {
		return payload
	}
	if payload.Action == "search" || payload.Action == "list" {
		content := usermemory.ParseContent(result)
		if content.Intro != "" || len(content.Sections) > 0 {
			payload.Content = &content
		}
	}
	return payload
}

func memoryToolAction(toolName string) string {
	if suffix, ok := strings.CutPrefix(strings.TrimSpace(strings.ToLower(toolName)), "memory."); ok {
		return suffix
	}
	return ""
}

func soulStreamPayload(toolName string, args map[string]interface{}, result string, isError bool) *ToolStreamSoulPayload {
	payload := &ToolStreamSoulPayload{}
	payload.Action = soulToolAction(toolName)
	if operation, ok := args["operation"].(string); ok {
		payload.Operation = strings.TrimSpace(strings.ToLower(operation))
	}
	if target, ok := args["target"].(string); ok {
		payload.Target = target
	}
	if anchor, ok := args["anchor"].(string); ok {
		payload.Anchor = anchor
	}
	if position, ok := args["position"].(string); ok {
		payload.Position = strings.TrimSpace(strings.ToLower(position))
	}
	if content, ok := args["content"].(string); ok && content != "" {
		payload.Content = content
	}
	if payload.Action == "read" && !isError {
		payload.Content = result
	}
	return payload
}

func soulToolAction(toolName string) string {
	if suffix, ok := strings.CutPrefix(strings.TrimSpace(strings.ToLower(toolName)), "soul."); ok {
		return suffix
	}
	return ""
}

// ModelMetrics holds performance data from a single LLM call.
type ModelMetrics struct {
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	TotalTokens      int     `json:"total_tokens,omitempty"`
	DurationMS       int64   `json:"duration_ms,omitempty"`
	TokensPerSecond  float64 `json:"tokens_per_second"`
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
	chatClient            llm.Chatter
	registry              *registry.Registry
	budget                promptbudget.ContextBudget
	model                 string
	soul                  *soul.Store
	userMemory            *usermemory.Store
	maxToolFailureRetries int
	requestTimeout        time.Duration
	log                   *config.Logger
}

// NewAgent initializes the Agent with an LLM chat client, tool registry, model name,
// soul store, SQLite user memory store, prompt budget, tool failure retry budget,
// and logger.
func NewAgent(
	chatClient llm.Chatter,
	registry *registry.Registry,
	model string,
	soul *soul.Store,
	userMemory *usermemory.Store,
	budget promptbudget.ContextBudget,
	maxToolFailureRetries int,
	requestTimeout time.Duration,
	log *config.Logger,
) *Agent {
	return &Agent{
		chatClient:            chatClient,
		registry:              registry,
		budget:                budget,
		model:                 model,
		soul:                  soul,
		userMemory:            userMemory,
		maxToolFailureRetries: maxToolFailureRetries,
		requestTimeout:        requestTimeout,
		log:                   log,
	}
}

func stripReplyContext(prompt string) (string, bool) {
	prompt = strings.TrimSpace(prompt)
	if !strings.HasPrefix(prompt, "[Replying ") {
		return prompt, false
	}
	parts := strings.SplitN(prompt, "\n\n", 2)
	if len(parts) < 2 {
		return "", true
	}
	return strings.TrimSpace(parts[1]), true
}

func sessionMemoryUserContent(prompt string, imageCount int) string {
	content, hadReplyContext := stripReplyContext(prompt)
	if content == "" && hadReplyContext {
		content = "[User replied to a prior message]"
	}
	if imageCount > 0 {
		content = strings.TrimSpace(content + fmt.Sprintf("\n\n[Attached %d image(s)]", imageCount))
	}
	return strings.TrimSpace(content)
}

// truncate returns s shortened to at most max runes, appending "..." if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
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
// injected into the request context so that tools such as memory.* can
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
func (a *Agent) Process(requestID string, gateway string, sessionKey string, senderID string, displayName string, userPrompt string, userImages []llm.InputImage, streamCallback func(chunk StreamChunk)) (*AgentResponse, error) {
	startedAt := time.Now()
	reqLog := a.log.Agent("agent", requestID, sessionKey, senderID, gateway, a.model)
	reqLog.Debug("agent.request.start", "agent request started",
		config.F("display_name", displayName),
		config.F("prompt_chars", len(userPrompt)),
		config.F("image_count", len(userImages)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), a.requestTimeout)
	defer cancel()

	// Inject the sender ID into the context so tool handlers can identify
	// which user this request belongs to without coupling to the session key.
	ctx = requestctx.WithSenderID(ctx, senderID)
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{
		RequestID: requestID,
		SessionID: sessionKey,
		SenderID:  senderID,
		Gateway:   gateway,
		Model:     a.model,
	})
	toolExposure := toolruntime.NewExposure()
	ctx = requestctx.WithToolExposer(ctx, toolExposure)

	// Read the soul file fresh on every request so that any edits the agent
	// made via the soul.* tools take effect immediately.
	soulContent, soulErr := a.soul.Read()
	if soulErr != nil {
		reqLog.Warn("agent.soul.read_failed", "failed to read soul file", config.ErrorField(soulErr))
	}

	// Build the dynamic system prompt: timestamp + soul + speaker identity.
	// Only system_rules memory is injected automatically here; relevant user and
	// session memories are added below as a structured retrieved-memory block.
	var promptParts []string
	promptParts = append(promptParts, "# Current Date and Time\n"+time.Now().UTC().Format(time.RFC1123))
	promptParts = append(promptParts, soulContent)

	speakerLine := a.currentSpeakerLine(reqLog, senderID)
	if speakerLine != "" {
		promptParts = append(promptParts, "# Current Speaker\n"+speakerLine)
	}
	requestUser := firstNonEmpty(speakerLine, displayName, senderID)
	promptParts = append(promptParts, a.userMemoryPromptSections(reqLog, senderID)...)

	dynamicSystemPrompt := strings.Join(promptParts, "\n\n")

	semanticQueryText, _ := stripReplyContext(userPrompt)
	if semanticQueryText == "" {
		semanticQueryText = userPrompt
	}
	if a.userMemory != nil {
		memoryContext, err := a.userMemory.BuildContext(ctx, senderID, sessionKey, semanticQueryText, usermemory.ContextOptions{
			RecentTurns:        memoryRecentTurns,
			ContextBudgetChars: int(float64(a.budget.PromptBudget()*4) * memoryContextBudgetRatio),
		})
		if err != nil {
			reqLog.Warn("agent.memory.context.failed", "failed to build retrieved memory context", config.F("status", "degraded"), config.ErrorField(err))
		} else if strings.TrimSpace(memoryContext.Block) != "" {
			dynamicSystemPrompt += "\n\n" + memoryContext.Block
			reqLog.Info("agent.memory.context.loaded", "loaded retrieved memory context",
				config.F("recent_turn_count", memoryContext.RecentTurnCount),
			)
		}
	}

	prune := promptbudget.Result{
		EstimatedBefore: promptbudget.EstimateTokens(dynamicSystemPrompt, nil, userPrompt, len(userImages), a.registry.LLMTools()),
		EstimatedAfter:  promptbudget.EstimateTokens(dynamicSystemPrompt, nil, userPrompt, len(userImages), a.registry.LLMTools()),
	}

	messages := make([]llm.ChatMessage, 0, 2)
	messages = append(messages, llm.ChatMessage{Role: "system", Content: dynamicSystemPrompt})
	messages = append(messages, llm.ChatMessage{Role: "user", Content: userPrompt, Images: userImages})

	if prune.EstimatedAfter > a.budget.PromptBudget() {
		reqLog.Warn("agent.context.over_budget", "prompt still exceeds budget after compaction",
			config.F("estimated_after", prune.EstimatedAfter),
			config.F("prompt_budget", a.budget.PromptBudget()),
		)
	}

	req := llm.ChatRequest{
		Model:  a.model,
		User:   requestUser,
		Stream: streamCallback != nil,
	}

	// Track accumulated thinking and content across all iterations.
	// The model may emit thinking tokens in any iteration; content tokens only
	// appear in the final response turn (when no tool calls are made).
	var accumulatedThinking strings.Builder
	var accumulatedContent strings.Builder
	toolExecutionCount := 0

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
	var chatCallback func(llm.ChatMessage)
	if streamCallback != nil {
		chatCallback = func(chunk llm.ChatMessage) {
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

	var lastResp *llm.ChatResponse
	toolFailureBudgetExhausted := false

	// Agentic loop: the model runs, may call tools, receives results, then runs again.
	// The loop exits when the model stops issuing tool calls, the request context
	// expires, or consecutive tool execution failures exhaust the retry budget.
	for iteration := 1; ; iteration++ {
		// Reset the content accumulator each iteration — we only keep the final
		// response turn's content. Thinking is accumulated across all iterations.
		accumulatedContent.Reset()

		req.Messages = messages
		req.Tools = a.registry.LLMToolsForVisibility(toolExposure.Visibility())
		reqLog.Debug("agent.model.call", "calling model",
			config.F("iteration", iteration),
			config.F("is_streaming", req.Stream),
			config.F("tool_count", len(req.Tools)),
		)

		resp, err := a.chatClient.Chat(ctx, req, chatCallback)
		if err != nil {
			reqLog.Error("agent.model.error", "model call failed", config.F("iteration", iteration), config.ErrorField(err))
			errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
			return &AgentResponse{
				Model:    a.model,
				Response: errorText,
				Error:    errorText,
			}, nil
		}

		lastResp = resp
		if iteration == 1 && resp.PromptTokens > 0 {
			reqLog.Debug("agent.context.estimated_vs_actual", "compared estimated and actual prompt tokens",
				config.F("estimated_after", prune.EstimatedAfter),
				config.F("actual_prompt_tokens", resp.PromptTokens),
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
			toolCallID := tc.ID
			if toolCallID == "" {
				toolCallID = fmt.Sprintf("call_%d_%d", iteration, toolExecutionCount+1)
			}
			toolStartedAt := time.Now()

			// Emit a structured tool-call chunk so UIs can render the invocation.
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkToolCall, Tool: toolStreamPayload(toolName, tc.Function.Arguments, "", 0, false)})
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
			toolExecutionCount++
			if streamCallback != nil {
				streamCallback(StreamChunk{
					Type: ChunkToolResult,
					Tool: toolStreamPayload(toolName, tc.Function.Arguments, toolContent, time.Since(toolStartedAt), execErr != nil),
				})
			}

			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				ToolName:   toolName,
				ToolCallID: toolCallID,
				Content:    toolContent,
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
			errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
			return &AgentResponse{
				Model:    a.model,
				Response: errorText,
				Error:    errorText,
			}, nil
		}

		lastResp = resp
		reqLog.Debug("agent.loop.complete", "completed agent loop after disabling tools",
			config.F("iteration_count", toolExecutionCount+1),
			config.F("failure_streak", consecutiveToolFailures),
			config.F("status", "degraded"),
		)
	}

	// Extract the final response content. The LLM client already handles
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

	userMemoryContent := sessionMemoryUserContent(userPrompt, len(userImages))
	if finalContent != "" && a.userMemory != nil {
		if err := a.userMemory.AppendSessionTurn(ctx, sessionKey, senderID, userMemoryContent, finalContent, toolAnnotations, sessionTurnTTL); err != nil {
			reqLog.Warn("agent.memory.session_write_failed", "failed to append session memory after turn", config.F("status", "degraded"), config.ErrorField(err))
		}
	}

	reqLog.Debug("agent.response.complete", "completed agent response",
		config.F("iteration_count", toolExecutionCount+1),
		config.F("response_chars", len(finalContent)),
		config.F("thinking_chars", len(finalThinking)),
		config.F("tool_call_count", toolExecutionCount),
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

	sections := make([]string, 0, 1)
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
