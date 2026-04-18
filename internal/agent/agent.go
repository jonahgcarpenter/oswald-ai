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
	promptDebugPath       string // directory for per-request prompt debug dumps; empty disables
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
	promptDebugPath string,
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
		promptDebugPath:       promptDebugPath,
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

func (a *Agent) compactTurnsToFit(ctx context.Context, systemPrompt string, turns []memory.Turn, userPrompt string, userImages []ollama.InputImage) ([]memory.Turn, memory.PromptPruneResult, bool) {
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
				a.log.Warn("Failed to compact %d turn(s) for prompt budget: %v", compactCount, err)
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
func (a *Agent) Process(sessionKey string, senderID string, displayName string, userPrompt string, userImages []ollama.InputImage, streamCallback func(chunk StreamChunk)) (*AgentResponse, error) {
	a.log.Debug("Processing request (session=%q sender=%q display=%q): %q", sessionKey, senderID, displayName, truncate(userPrompt, 100))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Inject the sender ID into the context so tool handlers can identify
	// which user this request belongs to without coupling to the session key.
	ctx = toolctx.WithSenderID(ctx, senderID)

	// Read the soul file fresh on every request so that any edits the agent
	// made via the soul_memory tool take effect immediately.
	soulContent, soulErr := a.soul.Read()
	if soulErr != nil {
		a.log.Warn("Failed to read soul file: %v", soulErr)
	}

	// Build the dynamic system prompt: soul + timestamp + speaker identity.
	// User memory is not injected automatically — the model retrieves it via
	// the persistent_memory tool when needed.
	var promptParts []string
	promptParts = append(promptParts, soulContent)
	promptParts = append(promptParts, "Current Date and Time: "+time.Now().Format(time.RFC1123))

	if speakerLine := a.currentSpeakerLine(senderID); speakerLine != "" {
		promptParts = append(promptParts, "## Current Speaker\n"+speakerLine)
	}
	promptParts = append(promptParts, a.userMemoryPromptSections(senderID)...)

	dynamicSystemPrompt := strings.Join(promptParts, "\n\n")

	// Retrieve retained conversation turns for this session. TTL and max-turn
	// pruning are applied destructively inside the store before history is used.
	turns := a.memory.Turns(sessionKey)

	// If the estimated prompt would exceed the active model budget, destructively
	// compact the oldest retained turns into a synthetic turn pair that remains
	// in ordinary conversation history.
	compactedTurns, prune, compacted := a.compactTurnsToFit(ctx, dynamicSystemPrompt, turns, userPrompt, userImages)
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

	// Write a full prompt debug dump when PROMPT_DEBUG_PATH is set.
	if a.promptDebugPath != "" {
		if dumpErr := debug.DumpPrompt(
			a.promptDebugPath,
			sessionKey,
			a.model,
			messages,
			a.registry.OllamaTools(),
			a.budget.ContextWindow,
			a.budget.PromptBudget(),
			prune.EstimatedBefore,
			prune.EstimatedAfter,
			prune.RemovedPairs,
			userImages,
		); dumpErr != nil {
			a.log.Warn("Prompt debug: failed to write dump: %v", dumpErr)
		} else {
			a.log.Debug("Prompt debug: wrote dump to %s", a.promptDebugPath)
		}
	}

	if len(trimmedHistory) > 0 {
		a.log.Debug("Memory: loaded %d historical message(s) for session %q", len(trimmedHistory), sessionKey)
	}
	if prune.RemovedPairs > 0 {
		a.log.Debug("Context budget: compacted %d turn pair(s) for model=%s budget=%d estimated_before=%d estimated_after=%d",
			prune.RemovedPairs, a.model, a.budget.PromptBudget(), prune.EstimatedBefore, prune.EstimatedAfter)
	}
	if prune.EstimatedAfter > a.budget.PromptBudget() {
		a.log.Warn("Context budget: prompt still exceeds budget for model=%s estimated=%d budget=%d after history compaction",
			a.model, prune.EstimatedAfter, a.budget.PromptBudget())
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

		resp, err := a.chatClient.Chat(ctx, req, chatCallback)
		if err != nil {
			a.log.Error("Model %s failed on iteration %d: %v", a.model, iteration, err)
			return &AgentResponse{
				Model:    a.model,
				Response: "Something broke, Try again or help fragsap buy a new GPU to fix these issues.",
				Error:    fmt.Sprintf("Model failed: %v", err),
			}, nil
		}

		lastResp = resp
		if iteration == 1 && resp.PromptEvalCount > 0 {
			a.log.Debug("Context budget: model=%s estimated_prompt_tokens=%d actual_prompt_tokens=%d",
				a.model, prune.EstimatedAfter, resp.PromptEvalCount)
		}

		a.log.Debug("Agentic loop iteration %d: tool_calls=%d thinking_len=%d content_len=%d failure_streak=%d",
			iteration, len(resp.Message.ToolCalls),
			len(resp.Message.Thinking), len(resp.Message.Content), consecutiveToolFailures)

		// No tool calls — the model is done. Exit the loop.
		if len(resp.Message.ToolCalls) == 0 {
			a.log.Debug("Agentic loop complete after %d iteration(s): model=%s", iteration, a.model)
			break
		}

		// Append the assistant turn (including its tool calls) to the conversation.
		messages = append(messages, resp.Message)

		// Execute each tool call and inject the results as tool response messages.
		// NOTE: Most models only emit one tool call at a time, but we handle
		// multiple to be safe.
		for _, tc := range resp.Message.ToolCalls {
			toolName := tc.Function.Name

			// Emit a status chunk so the gateway knows a tool is executing.
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkStatus, Text: fmt.Sprintf("[Calling: %s]", toolName)})
			}

			var toolContent string

			result, execErr := a.registry.Execute(ctx, toolName, tc.Function.Arguments)
			if execErr != nil {
				// Fail gracefully: inject the error so the model can recover.
				consecutiveToolFailures++
				a.log.Warn("Tool %q execution failed (%d/%d): %v", toolName, consecutiveToolFailures, a.maxToolFailureRetries, execErr)
				toolContent = fmt.Sprintf("Error: %v", execErr)
			} else {
				consecutiveToolFailures = 0
				toolContent = result
				a.log.Debug("Tool %q executed successfully", toolName)
				// Record a brief annotation for history storage.
				toolAnnotations = append(toolAnnotations, toolName)
			}

			messages = append(messages, ollama.ChatMessage{
				Role:     "tool",
				ToolName: toolName,
				Content:  toolContent,
			})

			if a.maxToolFailureRetries > 0 && consecutiveToolFailures >= a.maxToolFailureRetries {
				a.log.Warn("Agentic loop: stopping after %d consecutive tool execution failures", consecutiveToolFailures)
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
			a.log.Error("Model %s failed while finishing after tool failures: %v", a.model, err)
			return &AgentResponse{
				Model:    a.model,
				Response: "Something broke, Try again or help fragsap buy a new GPU to fix these issues.",
				Error:    fmt.Sprintf("Model failed: %v", err),
			}, nil
		}

		lastResp = resp
		a.log.Debug("Agentic loop complete after disabling tools following %d consecutive tool failures", consecutiveToolFailures)
	}

	a.log.Debug("Response complete: model=%s", a.model)

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

	// Persist the user prompt and the assistant's final response to memory.
	// Tool-call intermediaries are intentionally excluded to keep history lean —
	// only the conversational exchange (not the internal reasoning steps) is retained.
	// A brief tool-use annotation is appended to the assistant message so future
	// turns show what tools were called without ballooning history size.
	if finalContent != "" {
		storedContent := finalContent
		if len(toolAnnotations) > 0 {
			storedContent += "\n\n---\n_Tools used: " + strings.Join(toolAnnotations, ", ") + "_"
		}
		userMemoryContent := userPrompt
		if len(userImages) > 0 {
			userMemoryContent = strings.TrimSpace(userMemoryContent + fmt.Sprintf("\n\n[Attached %d image(s)]", len(userImages)))
		}
		a.memory.AppendTurn(
			sessionKey,
			ollama.ChatMessage{Role: "user", Content: userMemoryContent},
			ollama.ChatMessage{Role: "assistant", Content: storedContent},
		)
	}

	return &AgentResponse{
		Model:    a.model,
		Response: finalContent,
		Thinking: finalThinking,
		Metrics:  mapMetrics(lastResp),
	}, nil
}

func (a *Agent) currentSpeakerLine(senderID string) string {
	if senderID == "" {
		return ""
	}

	if a.userMemory != nil {
		intro, err := a.userMemory.ReadIntro(senderID)
		if err != nil {
			a.log.Warn("Failed to read user memory intro for %q: %v", senderID, err)
		} else if strings.TrimSpace(intro) != "" {
			return strings.TrimSpace(intro)
		}
	}

	return ""
}

func (a *Agent) userMemoryPromptSections(senderID string) []string {
	if senderID == "" || a.userMemory == nil {
		return nil
	}

	sections := make([]string, 0, 2)
	if identity := a.userMemoryPromptSection(senderID, "identity", "## User Identity Memory"); identity != "" {
		sections = append(sections, identity)
	}
	if systemRules := a.userMemoryPromptSection(senderID, "system_rules", "## User System Rules"); systemRules != "" {
		sections = append(sections, systemRules)
	}
	return sections
}

func (a *Agent) userMemoryPromptSection(senderID, category, heading string) string {
	content, err := a.userMemory.ReadCategory(senderID, category)
	if err != nil {
		a.log.Warn("Failed to read user memory category %q for %q: %v", category, senderID, err)
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
