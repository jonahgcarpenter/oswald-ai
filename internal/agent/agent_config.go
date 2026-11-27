package agent

import (
	"context"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	tokenbudget "github.com/jonahgcarpenter/oswald-ai/internal/session/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/skills"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/docache"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
	"strings"
)

type MarkdownSummarizer interface {
	Summarize(context.Context, []memory.SessionTurn) (string, *llm.ChatResponse, error)
	Get(string, string, int) string
	Set(string, string, int, string)
}

// targetAwareSummarizer compacts on the session's resolved model (issue #295).
// Holders implement it via SummarizeWithClient; test doubles may omit it and
// fall back to Summarize.
type targetAwareSummarizer interface {
	SummarizeWithTarget(context.Context, []memory.SessionTurn, Target, string) (string, *llm.ChatResponse, error)
}

// summarizeForTarget runs compaction on the session's resolved model when the
// summarizer supports it, else on its pinned model. previous carries the
// cached summary so compactions extend it instead of replacing it. Callers
// still own Get/Set.
func summarizeForTarget(ctx context.Context, summarizer MarkdownSummarizer, candidates []memory.SessionTurn, target Target, previous string) (string, *llm.ChatResponse, error) {
	if summarizer == nil {
		return "", nil, context.Canceled
	}
	if aware, ok := summarizer.(targetAwareSummarizer); ok {
		return aware.SummarizeWithTarget(ctx, candidates, target, previous)
	}
	return summarizer.Summarize(ctx, candidates)
}

// chatSessionID returns the stable per-conversation session identifier for
// providers requiring session affinity. Header injection fails closed.
func chatSessionID(sessionKey, requestID string) string {
	if id := strings.TrimSpace(sessionKey); id != "" && !strings.ContainsAny(id, "\r\n") {
		return id
	}
	if id := strings.TrimSpace(requestID); id != "" && !strings.ContainsAny(id, "\r\n") {
		return id
	}
	return ""
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
	mediaCache     *docache.Cache
	toolPolicy     governance.GlobalPolicy
	summarizer     MarkdownSummarizer
	workspaceRoot  string
	pluginSkills   []skills.PluginRoot
	pluginHooks    plugins.HookInvoker
	log            *config.Logger
	// router resolves the per-turn model target once at turn start. Nil keeps
	// the construction-pinned fields above, which is how delegate children run.
	router *ModelRouter
}

// SetPluginHooks installs the per-profile plugin hook host. Nil disables hooks.
func (a *Agent) SetPluginHooks(hooks plugins.HookInvoker) {
	if a != nil {
		a.pluginHooks = hooks
	}
}

// SetPluginSkills installs the read-only installed plugin skills roots bound
// into the prompt skill index. It mirrors the roots bound into the
// skill_manage tool handler so the index and the tool resolve the same set.
// Enablement is restart-classified, so construction-time roots never go stale.
func (a *Agent) SetPluginSkills(roots []skills.PluginRoot) {
	if a != nil {
		a.pluginSkills = roots
	}
}

// SetImageCache installs the private per-user image cache before requests start.
func (a *Agent) SetImageCache(cache *imagecache.Cache) {
	if a != nil {
		a.imageCache = cache
	}
}

// SetMediaCache installs the private per-user media cache so MEDIA: tokens can
// re-attach cached GIF/video/audio files.
func (a *Agent) SetMediaCache(cache *docache.Cache) {
	if a != nil {
		a.mediaCache = cache
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

// SetModelRouter installs the per-profile model router consulted once per
// turn. Nil keeps the construction-pinned model fields, which is how delegate
// children run.
func (a *Agent) SetModelRouter(router *ModelRouter) {
	if a != nil {
		a.router = router
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

// inputLimitForTarget derives the prompt budget for one turn from the resolved
// model's context window (issue #295). KnownWindow is harvested from live
// model listings with no config change; a miss falls back to the
// construction-pinned budget so turns never run unbounded. On a miss the
// router's listing is refreshed best-effort so the next turn sees the window.
func (a *Agent) inputLimitForTarget(ctx context.Context, target Target) int {
	if a != nil && a.router != nil && strings.TrimSpace(target.Provider) != "" && strings.TrimSpace(target.Model) != "" {
		if window, ok := a.router.KnownWindow(target.Provider, target.Model); ok && window > 0 {
			return tokenbudget.NewContextBudget(window).UsableInputLimit()
		}
		if models, err := a.router.ListModels(ctx, target.Provider); err == nil {
			for _, entry := range models {
				if strings.TrimSpace(entry.ID) == strings.TrimSpace(target.Model) && entry.Window > 0 {
					return tokenbudget.NewContextBudget(entry.Window).UsableInputLimit()
				}
			}
		}
	}
	if a == nil {
		return tokenbudget.NewContextBudget(0).UsableInputLimit()
	}
	return a.budget.UsableInputLimit()
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
// skipped; recording failures are debug-logged and never fail the turn. The
// caller supplies attribution explicitly: turn and compaction calls pass the
// resolved target for the session.
func (a *Agent) recordChatUsage(ctx context.Context, reqLog *config.Logger, owner, sessionKey string, generation int, resp *llm.ChatResponse, fallbackModel, billingProvider, billingBaseURL string) {
	if a == nil || a.userMemory == nil || resp == nil || generation <= 0 {
		return
	}
	model := strings.TrimSpace(resp.Model)
	if model == "" {
		model = strings.TrimSpace(fallbackModel)
	}
	if model == "" {
		return
	}
	record := memory.ModelUsageRecord{
		SessionID: sessionKey, UserID: owner, Generation: generation,
		Model: model, BillingProvider: strings.TrimSpace(billingProvider),
		BillingBaseURL:   strings.TrimSpace(billingBaseURL),
		ApiCalls:         1,
		PromptTokens:     max(0, resp.PromptTokens),
		CompletionTokens: max(0, resp.CompletionTokens),
	}
	if err := a.userMemory.RecordModelUsage(ctx, record); err != nil {
		reqLog.Debug("agent.session_memory.usage_failed", "failed to record session model usage",
			config.F("status", "degraded"), config.ErrorField(err))
	}
}
