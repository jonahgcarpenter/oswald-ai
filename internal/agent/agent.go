package agent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	toolruntime "github.com/jonahgcarpenter/oswald-ai/internal/tools/runtime"
)

const (
	sessionHistoryCandidateLimit = 100
	recentToolExposureTurns      = 4
	sessionTurnTTL               = 24 * time.Hour
	emptyResponseRetryPrompt     = "Your previous completion contained no visible response. Answer the user's last request now using only visible response content."
	emptyResponseFallback        = "I blanked on the actual answer. Try again and I'll take another shot."
	imageSizeFallback            = "Your image is too big. Crop it and try again."
	maxImageModelAttempts        = 5
	imageRetryScale              = 0.75
	imageInitialScaleMaxEdge     = 1920
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

// Request contains one fully resolved request submitted to the agent.
type Request struct {
	RequestID   string
	Principal   identity.Principal
	DisplayName string
	SessionKey  string
	Prompt      string
	Images      []llm.InputImage
	StreamFunc  func(StreamChunk)
}

// Agent handles LLM orchestration: a single agentic loop where the model
// calls tools from the registry and generates the final response.
type Agent struct {
	chatClient            llm.Chatter
	registry              *registry.Registry
	mcpProvider           MCPProvider
	budget                promptbudget.ContextBudget
	model                 string
	soul                  *soul.Store
	userMemory            *usermemory.Store
	maxToolFailureRetries int
	requestTimeout        time.Duration
	log                   *config.Logger
}

// MCPProvider resolves request-scoped MCP tools for the active canonical user.
type MCPProvider interface {
	DiscoveryTools(ctx context.Context, principal identity.Principal) []llm.Tool
	ResolveTools(ctx context.Context, principal identity.Principal, names []string) []string
	LLMTools(ctx context.Context, principal identity.Principal, exposed map[string]bool) []llm.Tool
	Execute(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}, exposed map[string]bool) (string, bool, error)
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
	mcpProviders ...MCPProvider,
) *Agent {
	var mcpProvider MCPProvider
	if len(mcpProviders) > 0 {
		mcpProvider = mcpProviders[0]
	}
	return &Agent{
		chatClient:            chatClient,
		registry:              registry,
		mcpProvider:           mcpProvider,
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

func providerUserValue(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "You are speaking with ")
	value = strings.TrimSuffix(value, ".")
	return strings.TrimSpace(value)
}

func gatewaySystemPrompt(gateway string) string {
	switch strings.TrimSpace(strings.ToLower(gateway)) {
	case "imessage":
		return "# Gateway Instructions\nThe user is reading this in iMessage, which does not render Markdown. Write responses in plain text. Do not use Markdown formatting such as **bold**, headings, tables, fenced code blocks, or inline code ticks. Use simple line breaks and plain bullets when helpful."
	default:
		return ""
	}
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

func (a *Agent) toolsForRequest(ctx context.Context, principal identity.Principal, exposure *toolruntime.Exposure) []llm.Tool {
	tools := a.registry.LLMToolsForVisibility(exposure.Visibility())
	if a.mcpProvider == nil {
		return tools
	}
	tools = append(tools, a.mcpProvider.DiscoveryTools(ctx, principal)...)
	tools = append(tools, a.mcpProvider.LLMTools(ctx, principal, exposure.ExposedMCPTools())...)
	return tools
}

func (a *Agent) executeTool(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}, exposure *toolruntime.Exposure) (string, error) {
	if a.mcpProvider != nil {
		if result, handled, err := a.mcpProvider.Execute(ctx, principal, name, args, exposure.ExposedMCPTools()); handled {
			return result, err
		}
	}
	return a.registry.Execute(ctx, name, args)
}

func (a *Agent) chatWithImageRetries(ctx context.Context, req llm.ChatRequest, callback func(llm.ChatMessage), log *config.Logger) (*llm.ChatResponse, error, bool) {
	originalMessages := req.Messages
	imageCount := 0
	for _, message := range originalMessages {
		imageCount += len(message.Images)
	}

	var firstErr error
	for attempt := 1; attempt <= maxImageModelAttempts; attempt++ {
		if imageCount > 0 {
			messages := append([]llm.ChatMessage(nil), originalMessages...)
			for i := range messages {
				if len(originalMessages[i].Images) == 0 {
					continue
				}
				resized, err := media.ResizeInputImagesForAttempt(originalMessages[i].Images, attempt, imageRetryScale, imageInitialScaleMaxEdge)
				if err != nil {
					log.Warn("agent.model.image_retry_resize_failed", "failed to resize images for model retry",
						config.F("attempt", attempt), config.F("image_count", imageCount),
						config.F("status", "degraded"), config.ErrorField(err))
					return nil, err, false
				}
				messages[i].Images = resized
			}
			req.Messages = messages
		}

		resp, err := a.chatClient.Chat(ctx, req, callback)
		if err == nil {
			return resp, nil, false
		}
		if imageCount == 0 || !llm.IsOllamaModelRunnerStoppedError(err) {
			return nil, err, false
		}
		if firstErr == nil {
			firstErr = err
		}
		if attempt == maxImageModelAttempts {
			log.Error("agent.model.image_retry_exhausted", "model runner stopped after resized image retries",
				config.F("attempt_count", attempt), config.F("image_count", imageCount),
				config.F("status", "error"), config.F("original_error", firstErr.Error()),
				config.F("last_error", err.Error()))
			return nil, err, true
		}
		log.Warn("agent.model.image_retry", "retrying model call with smaller images",
			config.F("attempt", attempt+1), config.F("image_count", imageCount),
			config.F("scale_percent", int(math.Pow(imageRetryScale, float64(attempt+1))*100)),
			config.F("status", "retry"))
	}
	return nil, firstErr, false
}

// Process handles the end-to-end agentic pipeline in a single loop.
// The model receives all registered tools and may call them zero or more times
// before generating its final response. Thinking tokens, content tokens, and
// agent status messages are streamed via streamCallback if provided.
//
// Tool execution errors are handled gracefully — failures inject an error tool
// response so the model can decide how to proceed. Provider errors are captured
// into AgentResponse.Error rather than returned as Go errors.
func (a *Agent) Process(request Request) (*AgentResponse, error) {
	if !request.Principal.Valid() {
		return nil, fmt.Errorf("agent request has no valid principal")
	}
	requestID := request.RequestID
	gateway := request.Principal.Gateway
	sessionKey := request.SessionKey
	senderID := request.Principal.CanonicalUserID
	displayName := request.DisplayName
	userPrompt := request.Prompt
	userImages := request.Images
	streamCallback := request.StreamFunc
	startedAt := time.Now()
	reqLog := a.log.Agent("agent", requestID, sessionKey, senderID, gateway, a.model)
	reqLog.Debug("agent.request.start", "agent request started",
		config.F("display_name", displayName),
		config.F("prompt_chars", len(userPrompt)),
		config.F("image_count", len(userImages)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), a.requestTimeout)
	defer cancel()

	// Inject the resolved actor so tool handlers derive ownership from the same
	// principal used by gateways, commands, and the broker.
	ctx = requestctx.WithPrincipal(ctx, request.Principal)
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{
		RequestID: requestID,
		SessionID: sessionKey,
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

	// Keep deployment policy separate from the frozen lower-authority tenant profile.
	var promptParts []string
	promptParts = append(promptParts, soulContent)
	if gatewayPrompt := gatewaySystemPrompt(gateway); gatewayPrompt != "" {
		promptParts = append(promptParts, gatewayPrompt)
	}
	dynamicSystemPrompt := strings.Join(promptParts, "\n\n")
	speakerLine := ""
	profileContent := ""
	sessionGeneration := 0
	if a.userMemory != nil {
		profile, err := a.userMemory.ResolveSessionProfile(ctx, senderID, sessionKey, sessionTurnTTL)
		if err != nil {
			reqLog.Error("agent.profile.load_failed", "failed to load tenant profile", config.F("status", "error"), config.ErrorField(err))
			return nil, fmt.Errorf("resolve tenant profile: %w", err)
		} else {
			speakerLine = profile.SpeakerIntro
			sessionGeneration = profile.Generation
			profileContent = profile.Content
			reqLog.Debug("agent.profile.loaded", "loaded frozen tenant profile",
				config.F("profile_version", profile.Version),
				config.F("latest_profile_version", profile.LatestVersion),
				config.F("profile_fact_count", profile.FactCount),
				config.F("profile_bytes", profile.Bytes),
				config.F("session_generation", profile.Generation),
				config.F("is_profile_new", profile.IsNewVersion),
				config.F("is_session_new", profile.IsNewSession),
			)
			if profile.IsNewVersion {
				reqLog.Info("agent.profile.version_advanced", "advanced tenant profile version",
					config.F("profile_version", profile.LatestVersion),
					config.F("profile_fact_count", profile.LatestFactCount),
					config.F("profile_bytes", profile.LatestBytes),
					config.F("status", "ok"),
				)
			}
			if profile.IsNewSession {
				reqLog.Info("agent.profile.session_bound", "bound tenant profile to session",
					config.F("profile_version", profile.Version),
					config.F("session_generation", profile.Generation),
					config.F("status", "ok"),
				)
			}
		}
	}
	requestUser := providerUserValue(firstNonEmpty(speakerLine, displayName, senderID))

	var recentTurns []usermemory.SessionTurn
	var recentToolNames []string
	if a.userMemory != nil && sessionGeneration > 0 {
		var err error
		recentTurns, err = a.userMemory.RecentCompletedExchanges(ctx, senderID, sessionKey, sessionGeneration, sessionHistoryCandidateLimit)
		if err != nil {
			reqLog.Warn("agent.memory.context.failed", "failed to build retrieved memory context", config.F("status", "degraded"), config.ErrorField(err))
			recentTurns = nil
		} else {
			toolTurnCount := len(recentTurns)
			if toolTurnCount > recentToolExposureTurns {
				toolTurnCount = recentToolExposureTurns
			}
			for _, turn := range recentTurns[:toolTurnCount] {
				recentToolNames = append(recentToolNames, turn.ToolNames...)
			}
			recentToolNames = uniqueToolNames(recentToolNames)
			reqLog.Debug("agent.memory.context.loaded", "loaded retrieved memory context",
				config.F("candidate_turn_count", len(recentTurns)),
			)
		}
	}
	if a.mcpProvider != nil && len(recentToolNames) > 0 {
		mcpCandidates := make([]string, 0, len(recentToolNames))
		for _, name := range recentToolNames {
			if !a.registry.HasHandler(name) {
				mcpCandidates = append(mcpCandidates, name)
			}
		}
		toolExposure.ExposeTools(a.mcpProvider.ResolveTools(ctx, request.Principal, mcpCandidates))
	}

	initialTools := a.toolsForRequest(ctx, request.Principal, toolExposure)
	promptContext := AssemblePromptContext(dynamicSystemPrompt, profileContent, userPrompt, userImages, recentTurns, initialTools, a.budget.UsableInputLimit())
	messages := promptContext.Messages
	if promptContext.RequiredOverBudget {
		reqLog.Warn("agent.context.over_budget", "prompt still exceeds budget after compaction",
			config.F("estimated_after", promptContext.EstimatedAfter),
			config.F("prompt_budget", promptContext.InputLimit),
		)
	}
	reqLog.Debug("agent.context.selected", "selected complete session exchanges",
		config.F("selected_turn_count", promptContext.SelectedTurnCount),
		config.F("omitted_turn_count", promptContext.OmittedTurnCount),
		config.F("estimated_before", promptContext.EstimatedBefore),
		config.F("estimated_after", promptContext.EstimatedAfter),
	)

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
	temporaryParserFallback := false
	imageSizeFallbackUsed := false

	// Agentic loop: the model runs, may call tools, receives results, then runs again.
	// The loop exits when the model stops issuing tool calls, the request context
	// expires, or consecutive tool execution failures exhaust the retry budget.
	for iteration := 1; ; iteration++ {
		// Reset the content accumulator each iteration — we only keep the final
		// response turn's content. Thinking is accumulated across all iterations.
		accumulatedContent.Reset()

		req.Messages = messages
		req.Tools = a.toolsForRequest(ctx, request.Principal, toolExposure)
		reqLog.Debug("agent.model.call", "calling model",
			config.F("iteration", iteration),
			config.F("is_streaming", req.Stream),
			config.F("tool_count", len(req.Tools)),
		)

		resp, err, imageRetriesExhausted := a.chatWithImageRetries(ctx, req, chatCallback, reqLog)
		if err != nil {
			reqLog.Error("agent.model.error", "model call failed", config.F("iteration", iteration), config.ErrorField(err))
			if imageRetriesExhausted {
				imageSizeFallbackUsed = true
				resp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: imageSizeFallback}}
				if streamCallback != nil {
					streamCallback(StreamChunk{Type: ChunkContent, Text: imageSizeFallback})
				}
			} else if llm.IsTemporaryOllamaToolParserError(err) {
				// Temporary workaround for an upstream Ollama/Qwen tool-markup parser
				// defect. Retry the identical request once and remove this branch when fixed.
				reqLog.Warn("agent.model.temporary_parser_retry", "retrying model call after upstream tool parser failure",
					config.F("iteration", iteration),
					config.F("retry_attempt", 1),
					config.F("status", "retry"),
				)
				resp, err = a.chatClient.Chat(ctx, req, chatCallback)
				if err == nil {
					reqLog.Warn("agent.model.temporary_parser_retry_recovered", "model call recovered after upstream tool parser failure",
						config.F("iteration", iteration),
						config.F("retry_attempt", 1),
						config.F("is_recovered", true),
						config.F("status", "degraded"),
					)
				} else {
					reqLog.Error("agent.model.temporary_parser_retry_failed", "model retry failed after upstream tool parser failure",
						config.F("iteration", iteration),
						config.F("retry_attempt", 1),
						config.F("is_recovered", false),
						config.F("status", "error"),
						config.ErrorField(err),
					)
					if llm.IsTemporaryOllamaToolParserError(err) {
						temporaryParserFallback = true
						resp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: emptyResponseFallback}}
						if streamCallback != nil {
							streamCallback(StreamChunk{Type: ChunkContent, Text: emptyResponseFallback})
						}
					} else {
						errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
						return &AgentResponse{Model: a.model, Response: errorText, Error: errorText}, nil
					}
				}
			} else {
				errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
				return &AgentResponse{Model: a.model, Response: errorText, Error: errorText}, nil
			}
		}

		lastResp = resp
		if iteration == 1 && resp.PromptTokens > 0 {
			reqLog.Debug("agent.context.estimated_vs_actual", "compared estimated and actual prompt tokens",
				config.F("estimated_after", promptContext.EstimatedAfter),
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
			reqLog.Info("agent.tool.start", "starting tool execution",
				config.F("iteration", iteration),
				config.F("tool_name", toolName),
			)

			var toolContent string

			result, execErr := a.executeTool(ctx, request.Principal, toolName, tc.Function.Arguments, toolExposure)
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

		resp, err, imageRetriesExhausted := a.chatWithImageRetries(ctx, finalReq, chatCallback, reqLog)
		if err != nil {
			reqLog.Error("agent.model.error", "model finish failed after tool failures", config.ErrorField(err))
			if imageRetriesExhausted {
				imageSizeFallbackUsed = true
				resp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: imageSizeFallback}}
				if streamCallback != nil {
					streamCallback(StreamChunk{Type: ChunkContent, Text: imageSizeFallback})
				}
			} else {
				errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
				return &AgentResponse{Model: a.model, Response: errorText, Error: errorText}, nil
			}
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
	if strings.TrimSpace(finalContent) == "" {
		retryMessages := append([]llm.ChatMessage{}, messages...)
		retryMessages = append(retryMessages, llm.ChatMessage{Role: "user", Content: emptyResponseRetryPrompt})

		accumulatedContent.Reset()
		retryReq := req
		retryReq.Messages = retryMessages
		retryReq.Tools = nil

		reqLog.Warn("agent.response.empty_retry", "model returned no visible response; retrying once",
			config.F("thinking_chars", len(finalThinking)),
			config.F("status", "retry"),
		)
		retryResp, err, imageRetriesExhausted := a.chatWithImageRetries(ctx, retryReq, chatCallback, reqLog)
		if err != nil {
			reqLog.Warn("agent.response.empty_retry_failed", "empty-response retry failed",
				config.F("status", "degraded"),
				config.ErrorField(err),
			)
			if imageRetriesExhausted {
				imageSizeFallbackUsed = true
				finalContent = imageSizeFallback
				if streamCallback != nil {
					streamCallback(StreamChunk{Type: ChunkContent, Text: imageSizeFallback})
				}
			}
		} else {
			lastResp = retryResp
			finalContent = accumulatedContent.String()
			if strings.TrimSpace(finalContent) == "" {
				finalContent = retryResp.Message.Content
			}
			if retryResp.Message.Thinking != "" && !strings.Contains(finalThinking, retryResp.Message.Thinking) {
				finalThinking += retryResp.Message.Thinking
			}
		}

		if strings.TrimSpace(finalContent) == "" {
			finalContent = emptyResponseFallback
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkContent, Text: finalContent})
			}
			reqLog.Warn("agent.response.empty_fallback", "using generic fallback after empty model response",
				config.F("status", "degraded"),
			)
		}
	}
	if lastResp != nil {
		messages = append(messages, lastResp.Message)
	}

	userMemoryContent := sessionMemoryUserContent(userPrompt, len(userImages))
	if finalContent != "" && a.userMemory != nil && sessionGeneration > 0 {
		if err := a.userMemory.AppendSessionTurnForGeneration(ctx, sessionKey, senderID, sessionGeneration, userMemoryContent, finalContent, toolAnnotations, sessionTurnTTL); err != nil {
			reqLog.Warn("agent.memory.session_write_failed", "failed to append session memory after turn", config.F("status", "degraded"), config.ErrorField(err))
		}
	}

	responseStatus := "ok"
	if temporaryParserFallback || imageSizeFallbackUsed {
		responseStatus = "degraded"
	}
	reqLog.Info("agent.response.complete", "completed agent response",
		config.F("iteration_count", toolExecutionCount+1),
		config.F("response_chars", len(finalContent)),
		config.F("thinking_chars", len(finalThinking)),
		config.F("tool_call_count", toolExecutionCount),
		config.F("duration_ms", time.Since(startedAt).Milliseconds()),
		config.F("tool_failure_budget_exhausted", toolFailureBudgetExhausted),
		config.F("status", responseStatus),
	)

	return &AgentResponse{
		Model:    a.model,
		Response: finalContent,
		Thinking: finalThinking,
		Metrics:  mapMetrics(lastResp),
	}, nil
}
