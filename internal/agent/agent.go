package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	tokenbudget "github.com/jonahgcarpenter/oswald-ai/internal/session/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/exposure"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	visionanalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/vision_analyze"
	websearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/web_search"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

const (
	sessionHistoryCandidateLimit  = 1000
	sessionTurnTTL                = 24 * time.Hour
	emptyResponseRetryPrompt      = "Your previous completion contained no visible response. Review the question and continue where you left off."
	emptyResponseFallback         = "I blanked on the actual answer. Try again and I'll take another shot."
	contextCompactionFallback     = "I cannot continue because I ran out of context. Any completed actions have not been undone. Try splitting the remaining task into smaller steps."
	generatedImagePartialResponse = "The model could not finish the request. The generated images were cached but not delivered; any completed actions have not been undone. Ask again to continue editing."
	sessionPromptPressurePrefix   = "session-prompt-pressure-v1"
)

// MarkdownSummarizer compacts a full session into markdown in one
// background chat call with zero tools. The in-memory cache needs no DDL.
type MarkdownSummarizer interface {
	Summarize(context.Context, []memory.SessionTurn) (string, *llm.ChatResponse, error)
	Get(string, string, int) string
	Set(string, string, int, string)
}

// Agent handles LLM orchestration: a single agentic loop where the model
// calls tools from the registry and generates the final response.
type Agent struct {
	chatClient  llm.Chatter
	registry    *registry.Registry
	mcpProvider MCPProvider
	budget      tokenbudget.ContextBudget
	model       string
	provider    string
	// billingBaseURL is the configured provider endpoint attributed on usage
	// ledger rows. Empty omits it; set via SetBillingBaseURL before serving.
	billingBaseURL string
	soul           *soul.Store
	userMemory     SessionStore
	fileMemory     *files.Store
	imageCache     *imagecache.Cache
	toolPolicy     governance.GlobalPolicy
	summarizer     MarkdownSummarizer
	workspaceRoot  string
	log            *config.Logger
}

// SetImageCache installs the private per-user image cache before requests start.
func (a *Agent) SetImageCache(cache *imagecache.Cache) {
	if a != nil {
		a.imageCache = cache
	}
}

// SetFileMemory installs the file-backed memory store before the agent starts serving work.
func (a *Agent) SetFileMemory(store *files.Store) {
	if a != nil {
		a.fileMemory = store
	}
}

// SetMarkdownSummarizer installs the markdown compactor before the agent starts serving work.
func (a *Agent) SetMarkdownSummarizer(summarizer MarkdownSummarizer) {
	if a != nil {
		a.summarizer = summarizer
	}
}

// SetBillingBaseURL installs the configured provider endpoint attributed on
// usage ledger rows before the agent starts serving work.
func (a *Agent) SetBillingBaseURL(url string) {
	if a != nil {
		a.billingBaseURL = url
	}
}

// SetWorkspace installs the profile working directory rendered in the system
// prompt and bound into workspace tool handlers at registry construction.
func (a *Agent) SetWorkspace(root string) {
	if a != nil {
		a.workspaceRoot = root
	}
}

// NewAgent initializes the Agent with an LLM chat client, tool registry, model
// name and configured provider key, soul store, SQLite user memory store,
// prompt budget, tool-governance policy, and logger.
func NewAgent(
	chatClient llm.Chatter,
	registry *registry.Registry,
	model string,
	provider string,
	soul *soul.Store,
	userMemory SessionStore,
	budget tokenbudget.ContextBudget,
	toolPolicy governance.GlobalPolicy,
	log *config.Logger,
	mcpProviders ...MCPProvider,
) *Agent {
	var mcpProvider MCPProvider
	if len(mcpProviders) > 0 {
		mcpProvider = mcpProviders[0]
	}
	return &Agent{
		chatClient:  chatClient,
		registry:    registry,
		mcpProvider: mcpProvider,
		budget:      budget,
		model:       model,
		provider:    provider,
		soul:        soul,
		userMemory:  userMemory,
		toolPolicy:  toolPolicy,
		log:         log,
	}
}

// Process handles the end-to-end agentic pipeline in a single loop.
// The model receives all registered tools and may call them zero or more times
// before generating its final response. Thinking tokens, content tokens, and
// agent status messages are streamed via streamCallback if provided.
//
// Tool execution errors are handled gracefully — failures inject an error tool
// response so the model can decide how to proceed. Provider errors are captured
// into Response.Error rather than returned as Go errors, except that completed
// generated images are finalized with a partial response. Cancellation still aborts.
// recordChatUsage persists one completed provider call into the session's
// model usage ledger. Stateless requests and synthetic fallback responses are
// skipped; recording failures are debug-logged and never fail the turn.
func (a *Agent) recordChatUsage(ctx context.Context, reqLog *config.Logger, owner, sessionKey string, generation int, resp *llm.ChatResponse) {
	if a == nil || a.userMemory == nil || resp == nil || generation <= 0 {
		return
	}
	model := strings.TrimSpace(resp.Model)
	if model == "" {
		model = strings.TrimSpace(a.model)
	}
	if model == "" {
		return
	}
	record := memory.ModelUsageRecord{
		SessionID: sessionKey, UserID: owner, Generation: generation,
		Model: model, BillingProvider: strings.TrimSpace(a.provider),
		BillingBaseURL:   strings.TrimSpace(a.billingBaseURL),
		ApiCalls:         1,
		PromptTokens:     max(0, resp.PromptTokens),
		CompletionTokens: max(0, resp.CompletionTokens),
	}
	if err := a.userMemory.RecordModelUsage(ctx, record); err != nil {
		reqLog.Debug("agent.session_memory.usage_failed", "failed to record session model usage",
			config.F("status", "degraded"), config.ErrorField(err))
	}
}

func (a *Agent) Process(ctx context.Context, request Request) (response *Response, processErr error) {
	if !request.Principal.Authenticated() {
		return nil, fmt.Errorf("agent request has no authenticated principal")
	}
	if !request.Stateless && len(request.ClientHistory) != 0 {
		return nil, fmt.Errorf("client history requires stateless mode")
	}
	historyContext, err := clientHistoryContext(request.ClientHistory)
	if err != nil {
		return nil, err
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
	reqLog := a.log.Agent("agent", requestID, senderID, gateway, a.model)
	if name := strings.TrimSpace(displayName); name != "" {
		reqLog = reqLog.With(config.F("user_id", name))
	}
	modelIterations, toolExecutionCount, toolBlockedCount := 0, 0, 0
	persistenceStatus := "not_attempted"
	usage := requestctx.UsageCollectorFromContext(ctx)
	usage.SetExecution(requestctx.ExecutionSnapshot{Model: a.model, PersistenceStatus: "unknown"})
	defer func() {
		kind, status := "answer", "ok"
		if processErr != nil {
			kind, status = "error", "error"
		}
		if errors.Is(processErr, context.Canceled) {
			kind, status = "canceled", "ok"
		}
		if response != nil {
			response.ToolExecutionCount, response.ToolBlockedCount = toolExecutionCount, toolBlockedCount
			if response.Kind == "" {
				response.Kind = "answer"
			}
			if response.Error != "" {
				response.Kind = "provider_error"
			}
			kind = response.Kind
			response.PersistenceStatus = persistenceStatus
			if kind != "answer" {
				status = "degraded"
			}
			if kind == "provider_error" {
				status = "error"
			}
		}
		usage.SetExecution(requestctx.ExecutionSnapshot{ToolExecutionCount: toolExecutionCount, BlockedCount: toolBlockedCount,
			PersistenceStatus: persistenceStatus, ResponseKind: kind, Model: a.model, IsComplete: true})
		reqLog.Debug("agent.response.complete", "completed agent generation", config.F("iteration_count", modelIterations),
			config.F("is_execution_complete", true), config.F("tool_blocked_count", toolBlockedCount),
			config.F("tool_execution_count", toolExecutionCount), config.F("duration_ms", time.Since(startedAt).Milliseconds()),
			config.F("response_kind", kind), config.F("persistence_status", persistenceStatus), config.F("status", status))
	}()
	reqLog.Debug("agent.request.start", "agent request started",
		config.F("prompt_chars", len(userPrompt)),
		config.F("image_count", len(userImages)),
	)

	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Inject the resolved actor so tool handlers derive ownership from the same
	// principal used by gateways, commands, and the broker.
	ctx = requestctx.WithPrincipal(ctx, request.Principal)
	formationSourceText, _ := stripReplyContext(userPrompt)
	inherited := requestctx.MetadataFromContext(ctx)
	inherited.RequestID, inherited.SessionID, inherited.Model, inherited.CurrentUserText = requestID, sessionKey, a.model, formationSourceText
	if inherited.Workload == "" {
		inherited.Workload = "foreground"
	}
	ctx = requestctx.WithMetadata(ctx, inherited)
	reqLog = reqLog.With(requestctx.LogFields(ctx)...)
	contextImages := make([]requestctx.InputImage, 0, len(userImages))
	for i, image := range userImages {
		contextImages = append(contextImages, requestctx.InputImage{ID: fmt.Sprintf("current-%d", i+1), MIMEType: image.MimeType, Data: image.Data, Source: image.Source})
	}
	ctx = requestctx.WithInputImages(ctx, contextImages)
	toolExposure := exposure.NewExposure()
	if strings.EqualFold(strings.TrimSpace(gateway), "openai") {
		toolExposure.HideBuiltins(imagegenerate.Name)
	}
	ctx = requestctx.WithToolExposer(ctx, toolExposure)
	ctx = workspace.WithReviewer(ctx, workspace.NewLLMReviewer(a.chatClient, a.model))
	// Parked approval asks surface while the tool call blocks: keep a
	// gateway-provided asker (real message on transports without live
	// progress), else fall back to a streaming status chunk.
	ctx = ensureEscalationNotify(ctx, streamCallback, gateway)
	toolGovernor := governance.New(a.toolPolicy)

	// Read the canonical user's operator-managed soul fresh on every request.
	soulStarted := time.Now()
	soulContent, soulErr := a.soul.Read(ctx, senderID)
	if soulErr != nil {
		reqLog.Warn("agent.soul.read_failed", "failed to read soul file", config.F("status", "error"), config.F("duration_ms", time.Since(soulStarted).Milliseconds()), config.ErrorField(soulErr))
		return nil, fmt.Errorf("read user soul: %w", soulErr)
	}
	reqLog.Debug("agent.soul.loaded", "loaded private soul file", config.F("duration_ms", time.Since(soulStarted).Milliseconds()), config.F("soul_chars", len([]rune(soulContent))))

	// The session-bound memory snapshot joins the operator soul in the system message.
	// Code-owned operating directives sit between the soul and the memory blocks.
	dynamicSystemPrompt := soulContent
	if directives := operatingDirectivesBlock(); directives != "" {
		if strings.TrimSpace(dynamicSystemPrompt) == "" {
			dynamicSystemPrompt = directives
		} else {
			dynamicSystemPrompt += "\n\n" + directives
		}
	}
	speakerLine := ""
	contextBlock := ""
	sessionGeneration := 0
	var sessionStartedAt time.Time
	if a.userMemory != nil && !request.Stateless {
		session, err := a.userMemory.ResolveSessionContext(ctx, senderID, sessionKey, sessionTurnTTL)
		if err != nil {
			return nil, fmt.Errorf("resolve tenant session: %w", err)
		} else {
			speakerLine = session.SpeakerIntro
			sessionGeneration = session.Generation
			sessionStartedAt = session.StartedAt
			reqLog.Debug("agent.session.loaded", "loaded tenant session context",
				config.F("session_generation", session.Generation),
				config.F("is_session_new", session.IsNewSession))
			if session.IsNewSession {
				reqLog.Debug("agent.session.bound", "bound tenant session context",
					config.F("session_generation", session.Generation))
			}
		}
	}
	if a.fileMemory != nil && !request.SkipMemory {
		filesStarted := time.Now()
		var userContent, memoryContent string
		bound := false
		if a.userMemory != nil && !request.Stateless && sessionGeneration > 0 {
			var err error
			userContent, memoryContent, bound, err = a.userMemory.SessionFileMemory(ctx, senderID, sessionKey, sessionGeneration)
			if err != nil {
				reqLog.Warn("agent.memory.files.bind_failed", "failed to load session file memory", config.F("status", "error"), config.F("duration_ms", time.Since(filesStarted).Milliseconds()), config.ErrorField(err))
				return nil, fmt.Errorf("load session file memory: %w", err)
			}
		}
		if !bound {
			var err error
			userContent, memoryContent, err = a.fileMemory.Read(ctx, senderID)
			if err != nil {
				reqLog.Warn("agent.memory.files.load_failed", "failed to load private memory files", config.F("status", "error"), config.F("duration_ms", time.Since(filesStarted).Milliseconds()), config.ErrorField(err))
				return nil, fmt.Errorf("read file memory: %w", err)
			}
			if a.userMemory != nil && !request.Stateless && sessionGeneration > 0 {
				userContent, memoryContent, err = a.userMemory.BindSessionFileMemory(ctx, senderID, sessionKey, sessionGeneration, userContent, memoryContent)
				if err != nil {
					reqLog.Warn("agent.memory.files.bind_failed", "failed to bind session file memory", config.F("status", "error"), config.F("duration_ms", time.Since(filesStarted).Milliseconds()), config.ErrorField(err))
					return nil, fmt.Errorf("bind session file memory: %w", err)
				}
			}
		}
		contextBlock = renderFileMemory(userContent, memoryContent)
		reqLog.Debug("agent.memory.files.loaded", "loaded private memory files", config.F("user_chars", len([]rune(userContent))), config.F("memory_chars", len([]rune(memoryContent))), config.F("is_session_snapshot", a.userMemory != nil && !request.Stateless && sessionGeneration > 0), config.F("duration_ms", time.Since(filesStarted).Milliseconds()))
	}
	appendContext := func(block string) {
		if block == "" {
			return
		}
		if strings.TrimSpace(contextBlock) == "" {
			contextBlock = block
		} else {
			contextBlock += "\n\n" + block
		}
	}
	appendContext(runtimeInfoBlock(request.Stateless, sessionStartedAt, time.Now(), time.Local, a.model, a.provider, gateway))
	appendContext(environmentInfoBlock(a.workspaceRoot))
	if !request.Stateless {
		appendContext(sessionContextBlock(gateway, request.ChatLabel, request.DisplayName))
	}
	appendContext(platformNotes(gateway))
	requestUser := providerUserValue(firstNonEmpty(speakerLine, displayName, senderID))
	meta := requestctx.MetadataFromContext(ctx)
	meta.SessionGeneration = sessionGeneration
	ctx = requestctx.WithMetadata(ctx, meta)
	if a.userMemory != nil && !request.Stateless && sessionGeneration > 0 && a.registry.HasHandler(imagegenerate.Name) {
		imagesStarted := time.Now()
		priorImages, err := a.userMemory.SessionImages(ctx, senderID, sessionKey, sessionGeneration)
		if err != nil {
			reqLog.Warn("agent.images.load_failed", "failed to load session images", config.F("status", "degraded"), config.ErrorField(err))
		} else {
			contextImages = append(contextImages, priorImages...)
			reqLog.Debug("agent.images.loaded", "loaded session images", config.F("image_count", len(priorImages)), config.F("duration_ms", time.Since(imagesStarted).Milliseconds()))
		}
		ctx = requestctx.WithInputImages(ctx, contextImages)
	}
	if gateway != "openai" && len(contextImages) > 0 && (a.registry.HasHandler(imagegenerate.Name) || a.registry.HasHandler(visionanalyze.Name)) {
		if a.imageCache == nil {
			return nil, fmt.Errorf("image cache is unavailable")
		}
		for i := range contextImages {
			if contextImages[i].Path != "" {
				if _, _, err := a.imageCache.Resolve(ctx, senderID, contextImages[i].Path); err != nil {
					return nil, fmt.Errorf("resolve image catalog source: %w", err)
				}
				continue
			}
			data, err := base64.StdEncoding.DecodeString(contextImages[i].Data)
			if err != nil {
				return nil, fmt.Errorf("decode image catalog source: %w", err)
			}
			assetID := contextImages[i].ID
			if contextImages[i].Source != "generated" {
				assetID = config.NewRequestID()
			}
			path, err := a.imageCache.SaveAsset(ctx, senderID, assetID, data, contextImages[i].MIMEType)
			if err != nil {
				return nil, fmt.Errorf("cache image catalog source: %w", err)
			}
			contextImages[i].Path = path
		}
		ctx = requestctx.WithInputImages(ctx, contextImages)
	}
	var recentTurns []memory.SessionTurn
	if a.userMemory != nil && !request.Stateless && sessionGeneration > 0 {
		var err error
		recentTurns, err = a.userMemory.RecentCompletedExchangesAfter(ctx, senderID, sessionKey, sessionGeneration, 0, sessionHistoryCandidateLimit)
		if err != nil {
			reqLog.Warn("agent.session_memory.context.failed", "failed to build session-memory context", config.F("status", "degraded"), config.ErrorField(err))
			recentTurns = nil
		} else {
			reqLog.Debug("agent.session_memory.context.loaded", "loaded session-memory context",
				config.F("candidate_turn_count", len(recentTurns)),
			)
		}
	}

	initialCatalog := a.toolsForRequest(ctx, request.Principal, toolExposure, request.EnabledToolsets)
	if request.Stateless {
		var choiceErr error
		initialCatalog, choiceErr = a.toolsForStatelessRequest(initialCatalog, request.ClientTools, request.ClientToolChoice)
		if choiceErr != nil {
			reqLog.Debug("agent.loop.client_tool_choice", "rejected stateless tool choice",
				config.F("mode", request.ClientToolChoice.Mode),
				config.ErrorField(choiceErr))
			errCode := "named_tool_missing"
			if choiceErr.Error() == "no_tools_available" {
				errCode = "no_tools_available"
			}
			return &Response{Kind: "invalid_tool_choice", Model: a.model, ToolChoiceError: errCode}, nil
		}
	}
	inputLimit := a.budget.UsableInputLimit()
	minimumTail := preservedRecentTailCount(recentTurns, inputLimit)
	modelUserPrompt := promptWithAttachedImages(userPrompt, contextImages[:len(userImages)])
	userMemoryContent := sessionMemoryUserContent(userPrompt, contextImages[:len(userImages)])
	_ = userMemoryContent
	cachedMarkdown := ""
	if a.summarizer != nil && !request.Stateless && sessionGeneration > 0 {
		cachedMarkdown = a.summarizer.Get(senderID, sessionKey, sessionGeneration)
	}
	promptContext := AssemblePromptContext(dynamicSystemPrompt, contextBlock, modelUserPrompt, userImages, cachedMarkdown, minimumTail, recentTurns, initialCatalog.Tools, inputLimit)
	// 70% trigger: blocking markdown compaction inside the lane head so the
	// next message in the same conversation queues behind it.
	if !request.Stateless && a.summarizer != nil && sessionGeneration > 0 && len(recentTurns) > 0 &&
		promptContext.EstimatedBefore*100 >= inputLimit*tokenbudget.CompactionTriggerPercent {
		candidates := tokenbudget.Candidates(recentTurns)
		if len(candidates) == 0 {
			reqLog.Debug("agent.context.compaction_skipped", "skipped session compaction: only newest turns present")
		} else {
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkStatus, Text: "Compacting context..."})
			}
			markdown, usage, compactErr := a.summarizer.Summarize(ctx, candidates)
			if compactErr != nil {
				reqLog.Warn("agent.context.compaction_failed", "failed to compact session context",
					config.F("status", "degraded"), config.ErrorField(compactErr))
			} else {
				a.summarizer.Set(senderID, sessionKey, sessionGeneration, markdown)
				if markErr := a.userMemory.MarkSessionTurnsCompacted(ctx, senderID, sessionKey, sessionGeneration, turnIDs(candidates)); markErr != nil {
					reqLog.Warn("agent.context.compaction_mark_failed", "failed to mark compacted turns",
						config.F("status", "degraded"), config.ErrorField(markErr))
				}
				a.recordChatUsage(ctx, reqLog, senderID, sessionKey, sessionGeneration, usage)
				cachedMarkdown = markdown
				promptContext = AssemblePromptContext(dynamicSystemPrompt, contextBlock, modelUserPrompt, userImages, cachedMarkdown, minimumTail, recentTurns, initialCatalog.Tools, inputLimit)
				reqLog.Debug("agent.context.compacted", "compacted session context to markdown",
					config.F("estimated_before", promptContext.EstimatedBefore),
					config.F("estimated_after", promptContext.EstimatedAfter),
					config.F("prompt_budget", inputLimit),
					config.F("compacted_turn_count", len(candidates)))
			}
		}
	}
	messages := promptContext.Messages
	if request.Stateless {
		if historyContext != "" {
			messages = append(messages[:len(messages)-1], llm.ChatMessage{Role: "user", Content: historyContext}, messages[len(messages)-1])
		}
	}
	if request.Stateless {
		promptContext.RequiredEstimate = tokenbudget.EstimateRequest(messages, initialCatalog.Tools)
		promptContext.EstimatedBefore = promptContext.RequiredEstimate
		promptContext.EstimatedAfter = promptContext.RequiredEstimate
		promptContext.RequiredOverBudget = promptContext.RequiredEstimate > inputLimit
		if promptContext.RequiredOverBudget {
			return &Response{Kind: "over_budget", Model: a.model}, nil
		}
	}
	if promptContext.RequiredOverBudget {
		reqLog.Warn("agent.context.over_budget", "prompt still exceeds budget after compaction",
			config.F("estimated_after", promptContext.EstimatedAfter),
			config.F("prompt_budget", promptContext.InputLimit),
		)
	}
	reqLog.Debug("agent.context.selected", "selected complete session exchanges",
		config.F("selected_turn_count", promptContext.SelectedTurnCount),
		config.F("omitted_turn_count", promptContext.OmittedTurnCount),
		config.F("is_summary_included", promptContext.SummaryIncluded),
		config.F("summary_chars", promptContext.SummaryChars),
		config.F("minimum_tail_count", promptContext.MinimumTailCount),
		config.F("estimated_before", promptContext.EstimatedBefore),
		config.F("estimated_after", promptContext.EstimatedAfter),
	)

	req := llm.ChatRequest{
		Model: a.model,
		User:  requestUser,
		// Model transport is independent of whether the gateway displays progress.
		Stream: true,
	}

	// Track accumulated thinking and content across all iterations.
	// The model may emit thinking tokens in any iteration; content tokens only
	// appear in the final response turn (when no tool calls are made).
	var accumulatedThinking strings.Builder
	var accumulatedContent strings.Builder

	// toolAnnotations collects brief notes about tools used this request.
	// These are appended to the stored assistant message so future turns
	// show what tools were called without ballooning history size.
	var toolAnnotations []string
	toolHistory := memory.EmptyToolHistory()
	// oswaldTimeline records server-side tool calls/results for API UIs.
	// It is populated whether or not a stream callback exists so stateless
	// non-streaming responses carry the same details timeline.
	var oswaldTimeline []OswaldToolEvent

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
	var outputAttachments []media.OutputAttachment
	var generatedImages []requestctx.InputImage
	generatedPreviewCount := 0
	imageHighwater := make(map[string]int)
	for _, image := range contextImages {
		if image.VersionHighwater > imageHighwater[image.ImageID] {
			imageHighwater[image.ImageID] = image.VersionHighwater
		}
	}
	toolGovernanceStopReason := ""
	temporaryParserFallback := false
	imageSizeFallbackUsed := false
	emptyRetryDone := false
	useFallback := func(content string) {
		accumulatedContent.Reset()
		accumulatedContent.WriteString(content)
		lastResp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: content}}
		if streamCallback != nil {
			streamCallback(StreamChunk{Type: ChunkContent, Text: content})
		}
	}

	// Agentic loop: the model runs, may call tools, receives results, then runs again.
	// The loop exits when the model stops issuing tool calls, the request context
	// expires, or request-local tool governance exhausts a safety budget.
	for iteration := 1; ; iteration++ {
		// Reset the content accumulator each iteration — we only keep the final
		// response turn's content. Thinking is accumulated across all iterations.
		accumulatedContent.Reset()
		// Synthetic fallback responses are built locally, never submitted to a
		// provider, and must not count as model calls in the usage ledger.
		respSynthetic := false

		catalog := a.toolsForRequest(ctx, request.Principal, toolExposure, request.EnabledToolsets)
		if request.Stateless {
			var choiceErr error
			catalog, choiceErr = a.toolsForStatelessRequest(catalog, request.ClientTools, request.ClientToolChoice)
			if choiceErr != nil {
				// The initial catalog already validated the choice; a later
				// failure means the offered catalog changed mid-request.
				// Degrade to no tools rather than failing streamed work.
				reqLog.Debug("agent.loop.client_tool_choice", "degraded stateless tool choice",
					config.F("iteration", iteration),
					config.F("mode", request.ClientToolChoice.Mode),
					config.ErrorField(choiceErr))
				catalog = offeredToolCatalog{Policies: make(map[string]governance.ToolPolicy)}
			}
		}
		req.Tools = catalog.Tools
		// Caller generation parameters apply to every provider call in the
		// stateless loop; sampling/stop shape each model invocation equally.
		req.Temperature, req.TopP = nil, nil
		req.FrequencyPenalty, req.PresencePenalty = nil, nil
		req.Seed, req.Stop = nil, nil
		if request.Stateless && request.ClientGeneration != nil {
			req.Temperature = request.ClientGeneration.Temperature
			req.TopP = request.ClientGeneration.TopP
			req.FrequencyPenalty = request.ClientGeneration.FrequencyPenalty
			req.PresencePenalty = request.ClientGeneration.PresencePenalty
			req.Seed = request.ClientGeneration.Seed
			req.Stop = request.ClientGeneration.Stop
			req.MaxTokens = request.ClientGeneration.MaxTokens
			req.Format = request.ClientGeneration.ResponseFormat
		}
		// Spec forcing is first-call only: later iterations revert to auto so
		// executed Oswald tools cannot force an endless tool-call loop.
		// Single-tool catalogs also disable parallel calls per provider
		// contract unless the caller overrode the preference explicitly.
		req.ToolChoice = ""
		req.ParallelToolCalls = request.ClientParallelToolCalls
		if request.Stateless && iteration == 1 && len(catalog.Tools) > 0 {
			switch request.ClientToolChoice.Mode {
			case "required", "named":
				req.ToolChoice = llm.ToolChoiceRequired
			case "allowed":
				if request.ClientToolChoice.AllowedMode == "required" {
					req.ToolChoice = llm.ToolChoiceRequired
				}
			}
			if req.ParallelToolCalls == nil {
				single := request.ClientToolChoice.Mode == "named" ||
					(request.ClientToolChoice.Mode == "allowed" && len(request.ClientToolChoice.Allowed) == 1)
				if single {
					disabled := false
					req.ParallelToolCalls = &disabled
				}
			}
		}
		req.Messages = messages
		if request.Stateless && tokenbudget.EstimateRequest(req.Messages, req.Tools) > inputLimit {
			return &Response{Kind: "over_budget", Model: a.model}, nil
		}
		reqLog.Debug("agent.model.call", "calling model",
			config.F("iteration", iteration),
			config.F("is_streaming", req.Stream),
			config.F("tool_count", len(req.Tools)),
		)

		modelIterations++
		resp, err, imageRetriesExhausted := a.chatWithImageRetries(ctx, req, chatCallback, reqLog)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if llm.IsContextLengthExceededError(err) && !request.Stateless && a.summarizer != nil && sessionGeneration > 0 && len(recentTurns) > 0 {
				if candidates := tokenbudget.Candidates(recentTurns); len(candidates) > 0 {
					if streamCallback != nil {
						streamCallback(StreamChunk{Type: ChunkStatus, Text: "Compacting context..."})
					}
					if markdown, usage, compactErr := a.summarizer.Summarize(ctx, candidates); compactErr == nil {
						a.summarizer.Set(senderID, sessionKey, sessionGeneration, markdown)
						if markErr := a.userMemory.MarkSessionTurnsCompacted(ctx, senderID, sessionKey, sessionGeneration, turnIDs(candidates)); markErr != nil {
							reqLog.Warn("agent.context.compaction_mark_failed", "failed to mark compacted turns",
								config.F("status", "degraded"), config.ErrorField(markErr))
						}
						a.recordChatUsage(ctx, reqLog, senderID, sessionKey, sessionGeneration, usage)
						rebuilt := AssemblePromptContext(dynamicSystemPrompt, contextBlock, modelUserPrompt, userImages, markdown, preservedRecentTailCount(recentTurns, inputLimit), recentTurns, req.Tools, inputLimit)
						messages = rebuilt.Messages
						if emptyRetryDone && (len(messages) == 0 || messages[len(messages)-1].Content != emptyResponseRetryPrompt) {
							messages = append(messages, llm.ChatMessage{Role: "user", Content: emptyResponseRetryPrompt})
						}
						req.Messages = messages
						reqLog.Warn("agent.context.provider_overflow_recovery", "retrying model call after provider context overflow",
							config.F("iteration", iteration), config.F("status", "retry"))
						modelIterations++
						resp, err, imageRetriesExhausted = a.chatWithImageRetries(ctx, req, chatCallback, reqLog)
					}
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			if err != nil && llm.IsContextLengthExceededError(err) {
				useFallback(contextCompactionFallback)
				goto finalize
			}
			if err == nil {
				// Continue with the response recovered after compaction.
				reqLog.Debug("agent.model.context_retry_recovered", "model recovered after context compaction")
			} else if len(generatedImages) > 0 && !llm.IsTemporaryOllamaToolParserError(err) {
				useFallback(generatedImagePartialResponse)
				goto finalize
			} else if imageRetriesExhausted {
				imageSizeFallbackUsed = true
				respSynthetic = true
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
				modelIterations++
				resp, err = a.chatClient.Chat(ctx, req, chatCallback)
				if errors.Is(err, context.Canceled) {
					return nil, err
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				if err == nil {
					reqLog.Debug("agent.model.temporary_parser_retry_recovered", "model call recovered after upstream tool parser failure",
						config.F("iteration", iteration),
						config.F("retry_attempt", 1),
						config.F("is_recovered", true),
						config.F("status", "degraded"),
					)
				} else {
					reqLog.Debug("agent.model.temporary_parser_retry_failed", "model retry failed after upstream tool parser failure",
						config.F("iteration", iteration),
						config.F("retry_attempt", 1),
						config.F("is_recovered", false),
						config.F("status", "error"),
						config.ErrorField(err),
					)
					if len(generatedImages) > 0 {
						if llm.IsContextLengthExceededError(err) {
							useFallback(contextCompactionFallback)
						} else {
							useFallback(generatedImagePartialResponse)
						}
						goto finalize
					}
					if llm.IsTemporaryOllamaToolParserError(err) {
						temporaryParserFallback = true
						respSynthetic = true
						resp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: emptyResponseFallback}}
						if streamCallback != nil {
							streamCallback(StreamChunk{Type: ChunkContent, Text: emptyResponseFallback})
						}
					} else {
						errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
						return &Response{Model: a.model, Response: errorText, Error: errorText}, nil
					}
				}
			} else if emptyRetryDone && len(generatedImages) == 0 {
				// The empty-response continue already spent the one extra model
				// call. A generic failure here maps to the empty fallback, not
				// a provider error, matching the previous retry semantics.
				reqLog.Warn("agent.response.empty_retry_failed", "empty-response retry failed",
					config.F("status", "degraded"),
					config.ErrorField(err),
				)
				useFallback(emptyResponseFallback)
				goto finalize
			} else {
				errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
				return &Response{Model: a.model, Response: errorText, Error: errorText}, nil
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		normalizeToolCallIDs(&resp.Message, iteration)
		lastResp = resp
		if !respSynthetic {
			a.recordChatUsage(ctx, reqLog, senderID, sessionKey, sessionGeneration, resp)
		}
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
		)

		// No tool calls — the model is done, unless it produced no visible
		// response. In that case inject a static continue prompt once and run
		// another iteration with the full tool catalog intact so tool-driven
		// work can continue.
		if len(resp.Message.ToolCalls) == 0 {
			visibleContent := accumulatedContent.String()
			if visibleContent == "" {
				visibleContent = resp.Message.Content
			}
			if strings.TrimSpace(visibleContent) == "" && !emptyRetryDone {
				emptyRetryDone = true
				messages = append(messages, llm.ChatMessage{Role: "user", Content: emptyResponseRetryPrompt})
				reqLog.Warn("agent.response.empty_retry", "model returned no visible response; retrying once",
					config.F("thinking_chars", len(resp.Message.Thinking)),
					config.F("status", "retry"),
				)
				continue
			}
			reqLog.Debug("agent.loop.complete", "agent loop completed", config.F("iteration_count", iteration))
			break
		}
		// Stateless API: caller-owned tools are never executed server-side.
		// Return them verbatim so the external app can run the standard loop.
		// Only currently offered tools qualify; disallowed or hallucinated
		// names fall through to governance below.
		if request.Stateless && len(request.ClientTools) > 0 {
			var clientCalls []llm.ToolCall
			for _, tc := range resp.Message.ToolCalls {
				if _, offered := catalog.Policies[tc.Function.Name]; !offered {
					continue
				}
				if a.isClientToolCall(request.ClientTools, tc.Function.Name) {
					clientCalls = append(clientCalls, tc)
				}
			}
			if len(clientCalls) > 0 {
				finalThinking := accumulatedThinking.String()
				if finalThinking == "" {
					finalThinking = resp.Message.Thinking
				}
				finalContent := accumulatedContent.String()
				if finalContent == "" {
					finalContent = resp.Message.Content
				}
				reqLog.Debug("agent.loop.client_tools", "returning caller-owned tool calls",
					config.F("iteration", iteration),
					config.F("tool_call_count", len(clientCalls)))
				response := &Response{Kind: "tool_calls", Model: a.model, Response: finalContent, Thinking: finalThinking, ToolCalls: clientCalls, OswaldTools: oswaldTimeline, ServiceTier: request.ServiceTier}
				response.setObservedUsage(usage.Snapshot())
				return response, nil
			}
		}
		iterationDecision := toolGovernor.BeginToolIteration()

		// Append the assistant turn (including its tool calls) to the conversation.
		messages = append(messages, resp.Message)

		// Execute each tool call and inject the results as tool response messages.
		// NOTE: Most models only emit one tool call at a time, but we handle
		// multiple to be safe.
		historyBatch := memory.ToolHistoryBatch{AssistantContent: resp.Message.Content}
		for _, tc := range resp.Message.ToolCalls {
			toolName := tc.Function.Name
			toolCallID := tc.ID
			toolStartedAt := time.Now()
			isClientTool := request.Stateless && a.isClientToolCall(request.ClientTools, toolName)

			// Emit a structured tool-call chunk so UIs can render the invocation.
			callPayload := toolStreamPayload(toolName, tc.Function.Arguments, "", 0, false)
			oswaldTimeline = append(oswaldTimeline, OswaldToolEvent{
				Name:      callPayload.Name,
				Phase:     "call",
				Arguments: callPayload.Arguments,
			})
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkToolCall, Tool: callPayload})
			}

			var toolContent string
			var execErr error
			result := governance.Result{}
			policy, advertised := catalog.Policies[toolName]
			decision := governance.Decision{ReasonCode: iterationDecision.ReasonCode}
			var guardAdvice governance.GuardrailAdvice
			var observeOut governance.ObserveResult
			if isClientTool {
				// Caller-owned tools never execute server-side; leave the
				// verbatim round-trip path to handle them. Mark allowed so
				// no governance state is touched.
				decision = governance.Decision{Allowed: true}
			} else if iterationDecision.Allowed {
				decision = toolGovernor.BeforeExecution(toolName, tc.Function.Arguments, policy, advertised)
			}
			if decision.Allowed && !isClientTool {
				toolExecutionCount++
				usage.SetExecution(requestctx.ExecutionSnapshot{ToolExecutionCount: toolExecutionCount, BlockedCount: toolBlockedCount, PersistenceStatus: "unknown", Model: a.model})
				toolMeta := requestctx.MetadataFromContext(ctx)
				toolMeta.ParentOperationID = toolMeta.OperationID
				toolMeta.OperationID = config.NewRequestID()
				reqLog.Debug("agent.tool.start", "starting authorized tool execution", config.F("tool_name", toolName))
				isGenerated := toolName == imagegenerate.Name
				var plannedImage requestctx.InputImage
				selectedSlot := -1
				if isGenerated {
					plannedImage, selectedSlot, execErr = planGeneratedImage(tc.Function.Arguments, contextImages, generatedImages)
					if execErr == nil {
						if plannedImage.ImageID == "" {
							plannedImage.ImageID = config.NewRequestID()
						}
						plannedImage.Version = imageHighwater[plannedImage.ImageID] + 1
						if a.userMemory != nil && sessionGeneration > 0 {
							plannedImage.Version, execErr = a.userMemory.ReserveImageVersion(ctx, senderID, sessionKey, sessionGeneration, plannedImage.ImageID, imageHighwater[plannedImage.ImageID])
						}
						if execErr == nil {
							imageHighwater[plannedImage.ImageID] = plannedImage.Version
						}
					}
				}
				if execErr == nil {
					result, execErr = a.executeTool(requestctx.WithMetadata(ctx, toolMeta), request.Principal, toolName, tc.Function.Arguments, toolExposure)
				}
				if execErr == nil && len(result.Images) > 0 {
					if toolName != visionanalyze.Name || len(result.Images) != 1 || len(result.Attachments) != 0 || result.Outcome != governance.OutcomeProductive {
						execErr = fmt.Errorf("tool returned unsupported model images")
					}
				}
				if execErr == nil && len(result.Attachments) > 0 {
					if gateway == "openai" {
						execErr = fmt.Errorf("attachments are not supported by this gateway")
					} else if isGenerated {
						// Generated images are never delivered automatically. The model
						// delivers one by emitting its MEDIA:/absolute/path token, which
						// finalization resolves against the managed cache. Cache the
						// original bytes for that path plus the model preview, and keep
						// logical-version bookkeeping so edits keep working.
						if result.Outcome != governance.OutcomeProductive {
							execErr = fmt.Errorf("tool returned attachments without a productive result")
						} else if len(result.Attachments) != 1 {
							execErr = fmt.Errorf("generation must return exactly one image")
						} else if attachmentErr := media.ValidateOutputAttachments(result.Attachments); attachmentErr != nil {
							execErr = fmt.Errorf("tool returned invalid attachments: %w", attachmentErr)
						} else {
							for _, attachment := range result.Attachments {
								normalized, err := media.NormalizeVisionImage(ctx, attachment.Data, attachment.MIMEType, nil)
								if err != nil {
									execErr = fmt.Errorf("normalize generated image: %w", err)
									break
								}
								image := plannedImage
								image.ID, image.MIMEType, image.Data, image.Source = config.NewRequestID(), normalized.Image.MimeType, normalized.Image.Data, "generated"
								image.VersionHighwater = image.Version
								var metadata map[string]json.RawMessage
								if err := json.Unmarshal([]byte(result.Content), &metadata); err != nil || metadata == nil {
									execErr = fmt.Errorf("invalid generated image metadata")
									break
								}
								if a.imageCache == nil {
									execErr = fmt.Errorf("image cache is unavailable")
									break
								}
								image.Path, err = a.imageCache.SaveAsset(ctx, senderID, image.ID, attachment.Data, attachment.MIMEType)
								if err != nil {
									execErr = fmt.Errorf("cache generated image: %w", err)
									break
								}
								preview := normalized.Image
								preview.Source = "generated"
								preview.Geometry = &llm.ImageGeometry{SourceWidth: normalized.OriginalWidth, SourceHeight: normalized.OriginalHeight, Width: normalized.Width, Height: normalized.Height}
								result.Images = []llm.InputImage{preview}
								result.Content = imagegenerate.ResultText(image.Path, preview.Geometry, normalized.DecodedFormat == "gif")
								generatedPreviewCount = min(generatedPreviewCount+1, maxToolResultImages)
								for i := range contextImages {
									if contextImages[i].ID == image.ParentSourceImageID && contextImages[i].ImageID == "" {
										contextImages[i].ImageID = image.ImageID
									}
								}
								if selectedSlot >= 0 {
									generatedImages[selectedSlot] = image
								} else {
									generatedImages = append(generatedImages, image)
								}
								contextImages = append([]requestctx.InputImage{image}, contextImages...)
								if len(contextImages) > 24 {
									// Keep selected generated outputs editable even after many retries.
									for i := len(contextImages) - 1; i >= 0; i-- {
										pinned := false
										for _, selected := range generatedImages {
											if selected.ID == contextImages[i].ID {
												pinned = true
												break
											}
										}
										if !pinned {
											contextImages = append(contextImages[:i], contextImages[i+1:]...)
											break
										}
									}
								}
								ctx = requestctx.WithInputImages(ctx, contextImages)
								reqLog.Debug("agent.images.generated", "normalized generated image for active context", config.F("image_bytes", normalized.NormalizedBytes), config.F("image_count", generatedPreviewCount), config.F("selected_image_count", len(generatedImages)), config.F("catalog_image_count", len(contextImages)))
							}
						}
					} else {
						candidate := append(append([]media.OutputAttachment(nil), outputAttachments...), result.Attachments...)
						if result.Outcome != governance.OutcomeProductive {
							execErr = fmt.Errorf("tool returned attachments without a productive result")
						} else if attachmentErr := media.ValidateOutputAttachments(candidate); attachmentErr != nil {
							execErr = fmt.Errorf("tool returned invalid attachments: %w", attachmentErr)
						} else {
							if execErr == nil {
								outputAttachments = candidate
							}
						}
					}
				}
				guardAdvice = toolGovernor.AfterCall(toolName, tc.Function.Arguments, decision.Fingerprint(), result, execErr)
				status, outcome := "ok", string(result.Outcome)
				if result.IsDegraded {
					status = "degraded"
				}
				if execErr != nil {
					status, outcome = "error", "error"
				}
				if errors.Is(execErr, context.Canceled) {
					status, outcome = "ok", "canceled"
				}
				scope := "mcp"
				if a.registry.HasHandler(toolName) {
					scope = "builtin"
				}
				reqLog.Info("tool.completed", "completed tool execution", config.F("tool_name", toolName), config.F("scope", scope),
					config.F("reason_code", result.ReasonCode), config.ErrorField(execErr),
					config.F("duration_ms", time.Since(toolStartedAt).Milliseconds()), config.F("outcome", outcome), config.F("status", status))
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
			} else if !isClientTool {
				toolBlockedCount++
				usage.SetExecution(requestctx.ExecutionSnapshot{ToolExecutionCount: toolExecutionCount, BlockedCount: toolBlockedCount, PersistenceStatus: "unknown", Model: a.model})
				toolContent = governanceResultText(decision.ReasonCode)
				safeName := toolName
				if !advertised {
					safeName = "unadvertised"
				}
				reqLog.Info("tool.blocked", "blocked tool execution",
					config.F("tool_name", safeName),
					config.F("reason_code", decision.ReasonCode), config.F("status", "rejected"))
			}
			if isClientTool {
				toolContent = "Tool call deferred to caller; not executed server-side."
			} else if decision.Allowed && execErr != nil {
				// Fail gracefully: inject the error so the model can recover.
				if !errors.Is(execErr, context.Canceled) {
					reqLog.Warn("tool.failed", "tool execution failed; model may recover", config.F("tool_name", toolName), config.F("status", "degraded"), config.ErrorField(execErr))
				}
				toolContent = "Error: " + truncate(config.SafeErrorText(execErr), 1000)
			} else if decision.Allowed {
				toolContent = result.Content
				status := "ok"
				if result.IsDegraded {
					status = "degraded"
				}
				reqLog.Debug("agent.tool.success", "tool execution succeeded",
					config.F("iteration", iteration),
					config.F("tool_name", toolName),
					config.F("tool_outcome", result.Outcome),
					config.F("reason_code", result.ReasonCode),
					config.F("is_degraded", result.IsDegraded),
					config.F("duration_ms", time.Since(toolStartedAt).Milliseconds()),
					config.F("status", status),
				)
				// Keep the successful-name projection for MCP continuity and legacy turns.
				toolAnnotations = append(toolAnnotations, toolName)
			}
			// modelContent is transient model-visible text (warns/notices/stubs).
			// toolContent stays the durable full text for history payloads.
			modelContent := toolContent
			if !isClientTool && decision.Allowed {
				failed := execErr != nil
				observeOut = toolGovernor.ObserveCall(toolName, tc.Function.Arguments, toolContent, toolCallID, failed, decision.Fingerprint())
				var warnSuffix string
				if guardAdvice.Warn() {
					warnSuffix = "\n\n" + governance.FormatGuardrailWarning(guardAdvice.Code, guardAdvice.Tool, guardAdvice.Count, guardAdvice.Message)
				}
				if observeOut.Stub != "" {
					modelContent = observeOut.Stub + warnSuffix
				} else {
					modelContent += warnSuffix
				}
				if observeOut.Notice != "" {
					modelContent += "\n\n" + observeOut.Notice
				}
			}
			resultName := toolName
			if decision.Allowed && execErr == nil && result.ResultName != "" {
				resultName = result.ResultName
			}
			resultPayload := toolStreamPayload(resultName, tc.Function.Arguments, toolContent, time.Since(toolStartedAt), execErr != nil || !decision.Allowed)
			if toolName == websearch.Name && resultPayload.WebSearch != nil {
				resultPayload.WebSearch.IsDegraded = result.IsDegraded
			}
			oswaldTimeline = append(oswaldTimeline, OswaldToolEvent{
				Name:       resultPayload.Name,
				Phase:      "result",
				ResultText: resultPayload.ResultText,
				IsError:    resultPayload.IsError,
				DurationMS: resultPayload.DurationMS,
				WebSearch:  resultPayload.WebSearch,
			})
			if streamCallback != nil {
				streamCallback(StreamChunk{
					Type: ChunkToolResult,
					Tool: resultPayload,
				})
			}

			var toolImages []llm.InputImage
			if decision.Allowed && execErr == nil {
				toolImages = result.Images
			}
			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				ToolName:   resultName,
				ToolCallID: toolCallID,
				Content:    modelContent,
				Images:     toolImages,
			})
			historyPolicy := policy.History.Effective()
			if !advertised {
				historyPolicy.Mode = governance.HistoryMetadata
				historyPolicy.SearchResult = false
			}
			if historyPolicy.Mode != governance.HistoryNone {
				historyBatch.Calls = append(historyBatch.Calls, persistedToolCall(tc, historyPolicy, decision, result, execErr, toolContent, time.Now().UTC()))
			}
			stats := toolGovernor.Stats(toolName)
			loggedToolName := toolName
			if !advertised {
				loggedToolName = "unadvertised"
			}
			reqLog.Debug("agent.tool.governance", "updated request-local tool governance",
				config.F("tool_name", loggedToolName),
				config.F("tool_attempt_count", stats.Attempts),
				config.F("tool_execution_count", stats.Executions),
				config.F("tool_productive_count", stats.Productive),
				config.F("tool_unproductive_count", stats.Unproductive),
				config.F("tool_failure_count", stats.Failures),
				config.F("tool_duplicate_count", stats.Duplicates),
				config.F("tool_blocked_count", stats.Blocked),
				config.F("guardrail_code", guardAdvice.Code),
				config.F("guardrail_count", guardAdvice.Count),
				config.F("guardrail_notice", observeOut.Kind))
			if toolGovernor.HaltDecision() != nil {
				break
			}
		}
		if len(historyBatch.Calls) > 0 {
			toolHistory.Batches = append(toolHistory.Batches, historyBatch)
		}
		messages = boundImageToolResults(messages)
		if reason := toolGovernor.GlobalStopReason(); reason != "" {
			toolGovernanceStopReason = reason
			reqLog.Debug("agent.tool_budget.exhausted", "tool governance budget exhausted",
				config.F("reason_code", reason),
				config.F("tool_execution_count", toolGovernor.TotalExecutions()),
				config.F("tool_iteration_count", toolGovernor.ToolIterations()),
				config.F("status", "degraded"))
			break
		}
	}

	if toolGovernanceStopReason != "" {
		accumulatedContent.Reset()
		finalReq := req
		finalReq.Tools = nil
		finalReq.Messages = messages
		if request.Stateless && tokenbudget.EstimateRequest(finalReq.Messages, nil) > inputLimit {
			return &Response{Kind: "over_budget", Model: a.model}, nil
		}

		modelIterations++
		resp, err, imageRetriesExhausted := a.chatWithImageRetries(ctx, finalReq, chatCallback, reqLog)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return nil, err
				}
				if llm.IsContextLengthExceededError(err) {
					useFallback(contextCompactionFallback)
					goto finalize
				} else if len(generatedImages) > 0 {
					useFallback(generatedImagePartialResponse)
					goto finalize
				} else if imageRetriesExhausted {
					imageSizeFallbackUsed = true
					resp = &llm.ChatResponse{Model: a.model, Message: llm.ChatMessage{Role: "assistant", Content: imageSizeFallback}}
					if streamCallback != nil {
						streamCallback(StreamChunk{Type: ChunkContent, Text: imageSizeFallback})
					}
				} else {
					errorText := config.SafeErrorText(fmt.Errorf("model failed: %w", err))
					return &Response{Model: a.model, Response: errorText, Error: errorText}, nil
				}
			}
		}

		lastResp = resp
		reqLog.Debug("agent.loop.complete", "completed agent loop after disabling tools",
			config.F("iteration_count", modelIterations),
			config.F("reason_code", toolGovernanceStopReason),
			config.F("status", "degraded"),
		)
	}

finalize:
	// Extract the final response content. The LLM client already handles
	// thinking-to-content promotion for non-streaming calls.
	// When progress is displayed, content is tracked separately via the callback.
	finalContent := accumulatedContent.String()
	if finalContent == "" && lastResp != nil {
		finalContent = lastResp.Message.Content
	}

	finalThinking := accumulatedThinking.String()
	if finalThinking == "" && lastResp != nil {
		finalThinking = lastResp.Message.Thinking
	}
	if strings.TrimSpace(finalContent) == "" {
		// The in-loop empty retry already gave the model one more turn with
		// tools intact. If it is still empty here, fall back without another
		// model call.
		if len(generatedImages) > 0 {
			useFallback(generatedImagePartialResponse)
			finalContent = generatedImagePartialResponse
		} else {
			finalContent = emptyResponseFallback
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkContent, Text: finalContent})
			}
			reqLog.Warn("agent.response.empty_fallback", "using generic fallback after empty model response",
				config.F("status", "degraded"),
			)
		}
	}
	// MEDIA:/absolute/path tokens in final model text are delivery directives:
	// resolve them against the sender's managed image cache, attach what
	// resolves, and persist the stripped text so tokens never re-fire.
	// Generated images have no automatic delivery; they attach only through these tokens.
	if strings.Contains(finalContent, "MEDIA:") {
		stripped, mediaAttachments := a.resolveResponseMedia(ctx, reqLog, gateway, senderID, finalContent, outputAttachments)
		finalContent = stripped
		outputAttachments = append(outputAttachments, mediaAttachments...)
		if strings.TrimSpace(finalContent) == "" && len(mediaAttachments) == 0 {
			finalContent = emptyResponseFallback
			if streamCallback != nil {
				streamCallback(StreamChunk{Type: ChunkContent, Text: finalContent})
			}
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if lastResp != nil {
		messages = append(messages, lastResp.Message)
	}
	for i := range generatedImages {
		generatedImages[i].VersionHighwater = imageHighwater[generatedImages[i].ImageID]
	}
	// Stored generated images stay in first-logical-production order. Storage ordinals instead
	// encode successful generation recency so the next default is the newest asset.
	imagesForStorage := make([]requestctx.InputImage, 0, len(generatedImages))
	for i := len(contextImages) - 1; i >= 0; i-- {
		for _, selected := range generatedImages {
			if selected.ID == contextImages[i].ID {
				imagesForStorage = append(imagesForStorage, selected)
				break
			}
		}
	}
	if len(generatedImages) > 0 && (a.userMemory == nil || sessionGeneration <= 0) {
		persistenceStatus = "failed"
		return nil, fmt.Errorf("persist generated images: session storage is unavailable")
	}
	var storedTurn memory.StoredSessionTurn
	// Session-row attributes are resolved before persistence. The system
	// prompt ledger stores the exact rendered system message alongside its
	// hex SHA-256, matching operator data.
	sessionModel := strings.TrimSpace(a.model)
	if lastResp != nil && strings.TrimSpace(lastResp.Model) != "" {
		sessionModel = strings.TrimSpace(lastResp.Model)
	}
	modelConfig, _ := json.Marshal(map[string]any{"gateway_runtime": map[string]any{"provider": strings.TrimSpace(a.provider), "base_url": strings.TrimSpace(a.billingBaseURL), "api_mode": "chat_completions"}})
	systemPromptText := ""
	if len(promptContext.Messages) > 0 && promptContext.Messages[0].Role == "system" {
		systemPromptText = promptContext.Messages[0].Content
	}
	systemPromptHash := ""
	if systemPromptText != "" {
		sum := sha256.Sum256([]byte(systemPromptText))
		systemPromptHash = hex.EncodeToString(sum[:])
	}
	chatType := "group"
	if request.IsDirect {
		chatType = "dm"
	}
	// The response kind doubles as the session activity description. It is
	// computed before persistence because it only reads settled loop outputs.
	responseKind := "answer"
	if toolGovernanceStopReason != "" {
		responseKind = "tool_limit"
	}
	if temporaryParserFallback {
		responseKind = "parser_fallback"
	}
	if imageSizeFallbackUsed {
		responseKind = "image_fallback"
	}
	if finalContent == contextCompactionFallback {
		responseKind = "context_fallback"
	}
	if finalContent == emptyResponseFallback && !temporaryParserFallback {
		responseKind = "empty_fallback"
	}
	if finalContent == generatedImagePartialResponse {
		responseKind = "image_partial"
	}
	// Attachments-only turns (MEDIA tokens stripped all visible text) still
	// persist so delivery gating and history stay consistent.
	if (finalContent != "" || len(outputAttachments) > 0) && a.userMemory != nil && sessionGeneration > 0 && !request.Stateless {
		persistenceStatus = "failed"
		storedReplay := memory.SessionTurn{UserText: userMemoryContent, AssistantText: finalContent, ToolNames: uniqueToolNames(toolAnnotations), ToolHistory: toolHistory}
		completedPressure := tokenbudget.EstimateCompletedRequest(promptContext.EstimatedBefore, storedReplay.UserText, memory.SessionTurnMessages(storedReplay))
		finishReason := "stop"
		completionTokens := 0
		if lastResp != nil {
			if strings.TrimSpace(lastResp.DoneReason) != "" {
				finishReason = strings.TrimSpace(lastResp.DoneReason)
			}
			completionTokens = lastResp.CompletionTokens
		}
		var err error
		storedTurn, err = a.userMemory.AppendPendingSessionTurn(ctx, memory.SessionTurnWrite{SessionID: sessionKey, UserID: senderID, Generation: sessionGeneration, UserText: userMemoryContent, AssistantText: finalContent, GroupGateway: meta.GroupGateway, GroupChatID: meta.GroupChatID, PublicUserText: meta.PublicUserText, ToolNames: toolAnnotations, History: toolHistory, Images: imagesForStorage, TTL: sessionTurnTTL, AssistantFinishReason: finishReason, AssistantReasoning: finalThinking, AssistantReasoningContent: finalThinking, AssistantTokenCount: completionTokens, UserPlatformMessageID: request.PlatformMessageID, Model: sessionModel, BillingProvider: strings.TrimSpace(a.provider), BillingBaseURL: strings.TrimSpace(a.billingBaseURL), ModelConfig: string(modelConfig), ChatID: strings.TrimSpace(request.ChatID), ChatType: chatType, ChatDisplayName: strings.TrimSpace(request.ChatDisplayName), Platform: strings.TrimSpace(gateway), TransportProfile: strings.TrimSpace(request.TransportProfile), PlatformUserID: strings.TrimSpace(request.Principal.ExternalID), PlatformDisplayName: strings.TrimSpace(displayName), SystemPromptHash: systemPromptHash, SystemPromptText: systemPromptText, ActivityDescription: responseKind, Pressure: memory.SessionPromptPressure{Tokens: completedPressure, Limit: promptContext.InputLimit, Version: promptPressureVersion(a.model, promptContext.InputLimit)}})
		if err != nil {
			reqLog.Warn("agent.session_memory.write_failed", "failed to append session memory after turn", config.F("status", "degraded"), config.ErrorField(err))
			if len(generatedImages) > 0 {
				return nil, fmt.Errorf("persist generated images: %w", err)
			}
		}
		if len(generatedImages) > 0 && storedTurn.ID == 0 {
			return nil, fmt.Errorf("persist generated images: session turn was not stored")
		}
		if storedTurn.ID > 0 {
			persistenceStatus = "pending"
			if len(generatedImages) > 0 {
				reqLog.Debug("agent.images.stored", "stored pending session images", config.F("image_count", len(generatedImages)))
			}
		}
	}

	responseStatus := "ok"
	if temporaryParserFallback || imageSizeFallbackUsed || toolGovernanceStopReason != "" || finalContent == contextCompactionFallback || finalContent == generatedImagePartialResponse {
		responseStatus = "degraded"
	}
	reqLog.Debug("agent.response.detail", "completed agent response",
		config.F("iteration_count", modelIterations),
		config.F("response_chars", len(finalContent)),
		config.F("thinking_chars", len(finalThinking)),
		config.F("tool_call_count", toolExecutionCount),
		config.F("duration_ms", time.Since(startedAt).Milliseconds()),
		config.F("status", responseStatus),
	)
	final := &Response{
		Kind:              responseKind,
		Model:             a.model,
		Response:          finalContent,
		Thinking:          finalThinking,
		OswaldTools:       oswaldTimeline,
		Metrics:           mapMetrics(lastResp),
		Attachments:       outputAttachments,
		SourceTurnID:      storedTurn.ID,
		SessionGeneration: storedTurn.Generation,
		ServiceTier:       request.ServiceTier,
	}
	if lastResp != nil {
		switch strings.TrimSpace(lastResp.DoneReason) {
		case "length", "content_filter":
			final.FinishReason = strings.TrimSpace(lastResp.DoneReason)
		}
	}
	final.setObservedUsage(usage.Snapshot())
	return final, nil
}
