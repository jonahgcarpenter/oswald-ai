package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
)

const (
	// maxIntelResults caps the total number of tool result payloads accumulated
	// across all tool calls in a single Process() invocation, to protect the
	// model's context window from bloat.
	maxIntelResults = 5
)

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
	chatClient    ollama.Chatter
	registry      *tools.Registry
	model         string
	systemPrompt  string
	maxIterations int
	log           *config.Logger
}

// NewAgent initializes the Agent with an Ollama chat client, tool registry, worker config,
// iteration cap, and logger. The single worker drives the entire agentic loop.
func NewAgent(
	chatClient ollama.Chatter,
	registry *tools.Registry,
	worker *WorkerAgent,
	maxIterations int,
	log *config.Logger,
) *Agent {
	return &Agent{
		chatClient:    chatClient,
		registry:      registry,
		model:         worker.ResolveModel(),
		systemPrompt:  worker.SystemPrompt,
		maxIterations: maxIterations,
		log:           log,
	}
}

// truncate returns s shortened to at most max runes, appending "…" if cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
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

// Process handles the end-to-end agentic pipeline in a single loop.
// The model receives all registered tools and may call them zero or more times
// (up to maxIterations) before generating its final response. Thinking tokens,
// content tokens, and agent status messages are streamed via streamCallback if provided.
//
// Tool execution errors are handled gracefully — failures inject an error tool
// response so the model can decide how to proceed. Provider errors are captured
// into AgentResponse.Error rather than returned as Go errors.
func (a *Agent) Process(userPrompt string, streamCallback func(chunk StreamChunk)) (*AgentResponse, error) {
	a.log.Debug("Processing request: %q", truncate(userPrompt, 100))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Inject the real-time date into the system prompt
	dynamicSystemPrompt := fmt.Sprintf("%s\n\nCurrent Date and Time: %s",
		a.systemPrompt,
		time.Now().Format(time.RFC1123),
	)

	messages := []ollama.ChatMessage{
		{Role: "system", Content: dynamicSystemPrompt},
		{Role: "user", Content: userPrompt},
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

	// totalToolCalls tracks how many tool executions have fired this request.
	// Caps total tool results to protect the model's context window from bloat.
	totalToolCalls := 0

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

	// Agentic loop: the model runs, may call tools, receives results, then runs again.
	// The loop exits when the model stops issuing tool calls or the iteration cap is hit.
	for iteration := 1; iteration <= a.maxIterations; iteration++ {
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

		a.log.Debug("Agentic loop iteration %d/%d: tool_calls=%d thinking_len=%d content_len=%d",
			iteration, a.maxIterations, len(resp.Message.ToolCalls),
			len(resp.Message.Thinking), len(resp.Message.Content))

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

			if totalToolCalls >= maxIntelResults {
				// Cap reached — inform the model rather than silently dropping results.
				a.log.Warn("Tool call cap (%d) reached, skipping %q", maxIntelResults, toolName)
				toolContent = fmt.Sprintf("Tool call cap of %d reached. No further tool calls will be executed.", maxIntelResults)
			} else {
				result, execErr := a.registry.Execute(ctx, toolName, tc.Function.Arguments)
				if execErr != nil {
					// Fail gracefully: inject the error so the model can recover.
					a.log.Warn("Tool %q execution failed: %v", toolName, execErr)
					toolContent = fmt.Sprintf("Error: %v", execErr)
				} else {
					totalToolCalls++
					toolContent = result
					a.log.Debug("Tool %q executed successfully (total calls: %d)", toolName, totalToolCalls)
				}
			}

			messages = append(messages, ollama.ChatMessage{
				Role:     "tool",
				ToolName: toolName,
				Content:  toolContent,
			})
		}
	}

	// If the loop exited at the cap, log a warning.
	if lastResp != nil && len(lastResp.Message.ToolCalls) > 0 {
		a.log.Warn("Agentic loop: max iterations (%d) reached without final response", a.maxIterations)
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

	return &AgentResponse{
		Model:    a.model,
		Response: finalContent,
		Thinking: finalThinking,
		Metrics:  mapMetrics(lastResp),
	}, nil
}
