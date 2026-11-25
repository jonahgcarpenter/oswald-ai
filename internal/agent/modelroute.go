package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

const (
	listingFetchTimeout     = 8 * time.Second
	listingFetchConcurrency = 4
)

// listingCacheTTL bounds how long a provider's listing (success or failure) is
// reused. It is a var so tests can shorten it.
var listingCacheTTL = 60 * time.Second

// ErrProviderUnknown reports a provider name that is not configured in the
// current view's endpoint map.
var ErrProviderUnknown = errors.New("provider is not configured")

// errModelsListUnavailable is returned when no listing seam was wired.
var errModelsListUnavailable = errors.New("models list unavailable")

// Target is the resolved model routing for one turn: consumed once at turn
// start and never re-read, so an in-flight turn finishes on it even if pins,
// overrides, or config change mid-turn.
type Target struct {
	Model          string
	Provider       string
	BillingBaseURL string
	Client         llm.Chatter
}

// DefaultTarget is the profile's default model transport as resolved from
// config at startup (or on hot rotation).
type DefaultTarget struct {
	Model      string
	Provider   string
	URL        string
	APIKey     string
	VirtualKey string
}

// ChannelOverride routes one platform channel to a provider/model pair.
// Populated from platforms.<name>.channel_overrides in a later phase;
// nil/empty maps in a view mean no overrides.
type ChannelOverride struct {
	Provider string
	Model    string
}

// View is the immutable snapshot of live model state the router resolves
// against. Swap replaces it wholesale (copy-on-write); Resolve reads one
// snapshot for its whole call. Callers must not mutate a View's maps after
// handing it to NewModelRouter or SwapView.
type View struct {
	Default   DefaultTarget
	Endpoints map[string]config.ProviderEndpoint    // provider name → transport
	Overrides map[string]map[string]ChannelOverride // gateway → chat id → override
}

// SessionPinStore is the narrow pin-persistence contract implemented by the
// session memory store. Corrupt or unreadable pins fail closed as errors and
// the router degrades to the next chain link. A store marks a value that
// exists but cannot be interpreted by returning an error that implements
// UnreadablePin() bool; the router then reports reason "unreadable" instead
// of "store_unavailable". Any other error is treated as store unavailability.
type SessionPinStore interface {
	SessionModel(ctx context.Context, owner, sessionKey string) (provider, model string, found bool, err error)
	SetSessionModel(ctx context.Context, owner, sessionKey, provider, model string) error
}

// LinkDegradation records why a chain link was skipped. Link ∈ {"pin","override"};
// Reason is a fixed vocabulary: "unreadable", "unknown_provider", "empty_model",
// "store_unavailable", "client_unavailable". The caller logs it; the router
// itself never logs.
type LinkDegradation struct {
	Link   string
	Reason string
}

// NewClientFunc constructs a chat client for a provider transport (startup
// wires llm.NewGatewayClient).
type NewClientFunc func(url, apiKey, virtualKey string) llm.Chatter

// ListModelsFunc fetches one provider's model listing (startup wires the
// llm.GatewayClient implementation). Nil makes every provider report
// unavailable instead of panicking.
type ListModelsFunc func(ctx context.Context, url, apiKey, virtualKey string) ([]llm.ModelEntry, error)

// ProviderStatus is one provider's listing availability for command display.
// ModelCount is the number of reported models when Available is true and 0
// otherwise.
type ProviderStatus struct {
	Name       string
	Available  bool
	ModelCount int
}

// listingCacheEntry is one provider's cached listing result. Failures are
// cached alongside successes so a dead provider is not re-hammered within
// listingCacheTTL.
type listingCacheEntry struct {
	models    []llm.ModelEntry
	err       error
	expiresAt time.Time
}

// ModelRouter resolves the model transport for one turn: conversation pin
// first, then channel override, then the profile default. It never fails;
// unusable links degrade forward and are reported as LinkDegradation values.
type ModelRouter struct {
	mu   sync.RWMutex
	view View

	defaultClient llm.Chatter
	// defaultURL and defaultAPIKey identify the startup-pinned transport the
	// constructor-provided defaultClient belongs to. A rotated default with a
	// different transport is constructed and cached like any other endpoint.
	defaultURL    string
	defaultAPIKey string
	newClient     NewClientFunc
	listModels    ListModelsFunc
	pins          SessionPinStore

	clientsMu sync.Mutex
	clients   map[string]llm.Chatter

	listingMu    sync.Mutex
	listingCache map[string]listingCacheEntry

	knownWindowsMu sync.Mutex
	knownWindows   map[string]int
}

// NewModelRouter builds a router over the startup view. defaultClient is the
// client already pinned to the startup default transport, reused whenever a
// resolved endpoint matches that transport; a rotated default transport is
// constructed through newClient instead. listModels may be nil, in which case
// every provider reports unavailable. pins may be nil; the pin link then
// degrades as "store_unavailable".
func NewModelRouter(view View, defaultClient llm.Chatter, newClient NewClientFunc, listModels ListModelsFunc, pins SessionPinStore) *ModelRouter {
	return &ModelRouter{
		view:          view,
		defaultClient: defaultClient,
		defaultURL:    view.Default.URL,
		defaultAPIKey: view.Default.APIKey,
		newClient:     newClient,
		listModels:    listModels,
		pins:          pins,
		clients:       make(map[string]llm.Chatter),
		listingCache:  make(map[string]listingCacheEntry),
		knownWindows:  make(map[string]int),
	}
}

// Resolve returns the routing for one turn and any links it skipped. The
// chain is conversation pin > channel override > profile default; every
// failure falls through to the next link, so the result is always usable.
// Missing overrides are normal and never reported. The router never logs.
func (r *ModelRouter) Resolve(ctx context.Context, owner, sessionKey, gateway, chatID string) (Target, []LinkDegradation) {
	view := r.snapshot()
	var degradations []LinkDegradation

	if target, ok, reason := r.resolvePin(ctx, view, owner, sessionKey); ok {
		return target, degradations
	} else if reason != "" {
		degradations = append(degradations, LinkDegradation{Link: "pin", Reason: reason})
	}

	if target, ok, reason := r.resolveOverride(view, gateway, chatID); ok {
		return target, degradations
	} else if reason != "" {
		degradations = append(degradations, LinkDegradation{Link: "override", Reason: reason})
	}

	return r.defaultTarget(view), degradations
}

// SwapView replaces the routing snapshot atomically. Resolves that already
// took a snapshot finish on it unchanged. An unchanged default transport
// keeps the startup client; a rotated default transport is constructed
// through the client seam and cached on first use.
func (r *ModelRouter) SwapView(view View) {
	r.mu.Lock()
	r.view = view
	r.mu.Unlock()
}

// FixedTarget wraps already-resolved fields into a Target for turns that have
// no router (for example delegate children pinned to their parent's model).
func FixedTarget(model, provider, billingBaseURL string, client llm.Chatter) Target {
	return Target{
		Model:          model,
		Provider:       provider,
		BillingBaseURL: billingBaseURL,
		Client:         client,
	}
}

// snapshot copies the current view under the read lock. The maps are shared
// by contract (copy-on-write), so the copy is a stable read for the call.
func (r *ModelRouter) snapshot() View {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.view
}

// resolvePin applies the conversation-pin link. The bool reports a usable
// target; a non-empty reason reports a degradation for the caller to log.
func (r *ModelRouter) resolvePin(ctx context.Context, view View, owner, sessionKey string) (Target, bool, string) {
	if r.pins == nil {
		return Target{}, false, "store_unavailable"
	}
	provider, model, found, err := r.pins.SessionModel(ctx, owner, sessionKey)
	if err != nil {
		if pinValueUnreadable(err) {
			return Target{}, false, "unreadable"
		}
		return Target{}, false, "store_unavailable"
	}
	if !found {
		return Target{}, false, ""
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	endpoint, ok := view.Endpoints[provider]
	if !ok {
		return Target{}, false, "unknown_provider"
	}
	if model == "" {
		return Target{}, false, "empty_model"
	}
	client, ok := r.clientFor(view, endpoint)
	if !ok {
		return Target{}, false, "client_unavailable"
	}
	return Target{
		Model:          model,
		Provider:       provider,
		BillingBaseURL: endpoint.URL,
		Client:         client,
	}, true, ""
}

// resolveOverride applies the channel-override link. A missing gateway or
// chat id is normal and returns no degradation.
func (r *ModelRouter) resolveOverride(view View, gateway, chatID string) (Target, bool, string) {
	override, ok := view.Overrides[gateway][chatID]
	if !ok {
		return Target{}, false, ""
	}
	provider := strings.TrimSpace(override.Provider)
	model := strings.TrimSpace(override.Model)
	endpoint, ok := view.Endpoints[provider]
	if !ok {
		return Target{}, false, "unknown_provider"
	}
	if model == "" {
		return Target{}, false, "empty_model"
	}
	client, ok := r.clientFor(view, endpoint)
	if !ok {
		return Target{}, false, "client_unavailable"
	}
	return Target{
		Model:          model,
		Provider:       provider,
		BillingBaseURL: endpoint.URL,
		Client:         client,
	}, true, ""
}

// defaultTarget is the always-available final link. Its client resolves
// through the same startup-reuse/construct-and-cache path as any endpoint:
// an unchanged default transport reuses the startup client, while a rotated
// default transport is constructed on demand. If a rotated default's client
// cannot be constructed (a wiring bug, since startup guarantees a default),
// the link falls back to the startup client so it stays always-usable.
func (r *ModelRouter) defaultTarget(view View) Target {
	client, ok := r.clientFor(view, config.ProviderEndpoint{URL: view.Default.URL, APIKey: view.Default.APIKey})
	if !ok {
		client = r.defaultClient
	}
	return Target{
		Model:          view.Default.Model,
		Provider:       view.Default.Provider,
		BillingBaseURL: view.Default.URL,
		Client:         client,
	}
}

// clientFor returns the startup client when the endpoint matches the startup
// default transport, otherwise a client cached by URL and key. Keyless
// endpoints (empty APIKey) are valid and cache under their own key. A false
// result means no client was constructed for this endpoint (nil seam or nil
// construction result); callers must degrade the link instead of running the
// turn on the wrong transport.
func (r *ModelRouter) clientFor(view View, endpoint config.ProviderEndpoint) (llm.Chatter, bool) {
	if endpoint.URL == r.defaultURL && endpoint.APIKey == r.defaultAPIKey {
		return r.defaultClient, true
	}
	cacheKey := endpoint.URL + "\x00" + endpoint.APIKey
	r.clientsMu.Lock()
	defer r.clientsMu.Unlock()
	if client, ok := r.clients[cacheKey]; ok {
		return client, true
	}
	if r.newClient == nil {
		return nil, false
	}
	client := r.newClient(endpoint.URL, endpoint.APIKey, view.Default.VirtualKey)
	if client == nil {
		return nil, false
	}
	r.clients[cacheKey] = client
	return client, true
}

// pinValueUnreadable reports whether err describes a stored pin value that
// exists but cannot be interpreted, as opposed to cancellation or a store
// failure. Stores mark such values through the duck-typed UnreadablePin
// method, so the memory package needs no dependency on this package; decoder
// classifications are honored as a fallback for stores that wrap the cause.
func pinValueUnreadable(err error) bool {
	if err == nil {
		return false
	}
	var marked interface{ UnreadablePin() bool }
	if errors.As(err, &marked) {
		return marked.UnreadablePin()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch config.ErrorCode(err) {
	case "json_syntax", "json_type", "json_invalid_target", "invalid_argument":
		return true
	}
	return false
}

// ListProviders reports every configured provider's listing availability. It
// snapshots the view once, fetches uncached listings with bounded parallelism
// (listingFetchConcurrency, listingFetchTimeout each), caches successes and
// failures for listingCacheTTL, and sorts the result by provider name. A
// failed fetch degrades to Available:false with no error; the method never
// logs.
func (r *ModelRouter) ListProviders(ctx context.Context) []ProviderStatus {
	view := r.snapshot()
	names := make([]string, 0, len(view.Endpoints))
	for name := range view.Endpoints {
		names = append(names, name)
	}
	sort.Strings(names)

	statuses := make([]ProviderStatus, len(names))
	semaphore := make(chan struct{}, listingFetchConcurrency)
	var wg sync.WaitGroup
	for index, name := range names {
		index, name := index, name
		wg.Add(1)
		go func() {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			models, err := r.providerModels(ctx, view, name, view.Endpoints[name])
			statuses[index] = ProviderStatus{
				Name:       name,
				Available:  err == nil,
				ModelCount: len(models),
			}
		}()
	}
	wg.Wait()
	return statuses
}

// ListModels returns one provider's model listing through the shared cache.
// A provider name that is not in the current view's endpoint map wraps
// ErrProviderUnknown. Fetch failures are returned unchanged; the listing seam
// already launders endpoint and credential data out of them.
func (r *ModelRouter) ListModels(ctx context.Context, provider string) ([]llm.ModelEntry, error) {
	provider = strings.TrimSpace(provider)
	view := r.snapshot()
	endpoint, ok := view.Endpoints[provider]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrProviderUnknown, provider)
	}
	return r.providerModels(ctx, view, provider, endpoint)
}

// HasProvider reports whether the current view configures the provider.
func (r *ModelRouter) HasProvider(provider string) bool {
	_, ok := r.snapshot().Endpoints[strings.TrimSpace(provider)]
	return ok
}

// Providers returns the configured provider names sorted alphabetically. It
// performs no fetch, so command validation stays cheap.
func (r *ModelRouter) Providers() []string {
	view := r.snapshot()
	names := make([]string, 0, len(view.Endpoints))
	for name := range view.Endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// KnownWindow reports the last successfully harvested context window for a
// provider/model pair. Windows are sticky for the router's lifetime (no
// expiry) and are populated by successful listing fetches.
func (r *ModelRouter) KnownWindow(provider, model string) (int, bool) {
	r.knownWindowsMu.Lock()
	defer r.knownWindowsMu.Unlock()
	window, ok := r.knownWindows[provider+"\x00"+model]
	return window, ok
}

// providerModels serves a provider's listing from the TTL cache or fetches it.
// The cache holds failures too, so a dead provider is not re-fetched on every
// command. A nil listing seam reports unavailable without caching.
func (r *ModelRouter) providerModels(ctx context.Context, view View, provider string, endpoint config.ProviderEndpoint) ([]llm.ModelEntry, error) {
	if r.listModels == nil {
		return nil, errModelsListUnavailable
	}

	r.listingMu.Lock()
	r.pruneListingsLocked()
	if entry, ok := r.listingCache[provider]; ok {
		r.listingMu.Unlock()
		return entry.models, entry.err
	}
	r.listingMu.Unlock()

	fetchCtx, cancel := context.WithTimeout(ctx, listingFetchTimeout)
	defer cancel()
	models, err := r.listModels(fetchCtx, endpoint.URL, endpoint.APIKey, view.Default.VirtualKey)
	r.storeListing(provider, models, err)
	return models, err
}

// storeListing caches one fetch result and harvests context windows from
// successful listings. Windows never expire; issue #295 consumes them.
func (r *ModelRouter) storeListing(provider string, models []llm.ModelEntry, err error) {
	if err == nil {
		r.recordWindows(provider, models)
	}

	r.listingMu.Lock()
	defer r.listingMu.Unlock()
	if r.listingCache == nil {
		r.listingCache = make(map[string]listingCacheEntry)
	}
	r.pruneListingsLocked()
	r.listingCache[provider] = listingCacheEntry{
		models:    models,
		err:       err,
		expiresAt: time.Now().Add(listingCacheTTL),
	}
}

func (r *ModelRouter) recordWindows(provider string, models []llm.ModelEntry) {
	r.knownWindowsMu.Lock()
	defer r.knownWindowsMu.Unlock()
	if r.knownWindows == nil {
		r.knownWindows = make(map[string]int)
	}
	for _, model := range models {
		if model.Window > 0 {
			r.knownWindows[provider+"\x00"+model.ID] = model.Window
		}
	}
}

func (r *ModelRouter) pruneListingsLocked() {
	now := time.Now()
	for provider, entry := range r.listingCache {
		if !entry.expiresAt.After(now) {
			delete(r.listingCache, provider)
		}
	}
}
