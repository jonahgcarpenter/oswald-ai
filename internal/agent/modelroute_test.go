package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Synthetic router fixtures. URLs and keys are obviously fake; no credential
// value asserted here is real.
const (
	routerDefaultURL   = "https://gateway.example.com/v1"
	routerDefaultKey   = "fake-default-key"
	routerAlternateURL = "https://alt.example.com/v1"
	routerAlternateKey = "fake-alt-key"
	routerSecondKey    = "fake-second-key"
	routerLocalURL     = "http://localhost:11555/v1"
	routerRotatedURL   = "https://rotated.example.com/v1"
	routerRotatedKey   = "fake-rotated-key"
	routerVirtualKey   = "fake-virtual-key"
)

// routerTestChatter is a no-op Chatter identity token; tests compare pointers.
type routerTestChatter struct {
	label string
}

func (c *routerTestChatter) Chat(context.Context, llm.ChatRequest, func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{}, nil
}

// routerClientCall records one constructor invocation.
type routerClientCall struct {
	url        string
	apiKey     string
	virtualKey string
	client     llm.Chatter
}

// routerClientFactory is a concurrency-safe NewClientFunc double that returns
// one stable client instance per (url, key) pair. URLs in failURLs simulate a
// constructor that cannot build a client.
type routerClientFactory struct {
	mu       sync.Mutex
	calls    []routerClientCall
	clients  map[string]*routerTestChatter
	failURLs map[string]bool
}

func newRouterClientFactory() *routerClientFactory {
	return &routerClientFactory{clients: map[string]*routerTestChatter{}}
}

func (f *routerClientFactory) newClient(url, apiKey, virtualKey string) llm.Chatter {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failURLs[url] {
		f.calls = append(f.calls, routerClientCall{url: url, apiKey: apiKey, virtualKey: virtualKey})
		return nil
	}
	key := url + "\x00" + apiKey
	client, ok := f.clients[key]
	if !ok {
		client = &routerTestChatter{label: key}
		f.clients[key] = client
	}
	f.calls = append(f.calls, routerClientCall{url: url, apiKey: apiKey, virtualKey: virtualKey, client: client})
	return client
}

func (f *routerClientFactory) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *routerClientFactory) call(t *testing.T, index int) routerClientCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.calls) {
		t.Fatalf("constructor call %d missing (have %d)", index, len(f.calls))
	}
	return f.calls[index]
}

// routerStoredPin is one fake persisted pin.
type routerStoredPin struct {
	provider string
	model    string
}

// routerPinStore is an in-memory SessionPinStore double.
type routerPinStore struct {
	mu   sync.Mutex
	pins map[string]routerStoredPin
	err  error
}

func newRouterPinStore() *routerPinStore {
	return &routerPinStore{pins: map[string]routerStoredPin{}}
}

func (s *routerPinStore) set(owner, sessionKey, provider, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pins[owner+"\x00"+sessionKey] = routerStoredPin{provider: provider, model: model}
}

func (s *routerPinStore) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *routerPinStore) SessionModel(_ context.Context, owner, sessionKey string) (string, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", "", false, s.err
	}
	pin, ok := s.pins[owner+"\x00"+sessionKey]
	if !ok {
		return "", "", false, nil
	}
	return pin.provider, pin.model, true, nil
}

func (s *routerPinStore) SetSessionModel(_ context.Context, owner, sessionKey, provider, model string) error {
	s.set(owner, sessionKey, provider, model)
	return nil
}

// routerUnreadablePinError marks a stored value that cannot be interpreted.
type routerUnreadablePinError struct{ cause error }

func (e routerUnreadablePinError) Error() string {
	if e.cause == nil {
		return "unreadable session model pin"
	}
	return "unreadable session model pin: " + e.cause.Error()
}

func (e routerUnreadablePinError) Unwrap() error { return e.cause }

func (e routerUnreadablePinError) UnreadablePin() bool { return true }

// routerTestView is the base router view with a default transport plus
// alternate, second-key, and keyless providers.
func routerTestView() View {
	return View{
		Default: DefaultTarget{
			Model:      "default-model",
			Provider:   "default-provider",
			URL:        routerDefaultURL,
			APIKey:     routerDefaultKey,
			VirtualKey: routerVirtualKey,
		},
		Endpoints: map[string]config.ProviderEndpoint{
			"default-provider": {URL: routerDefaultURL, APIKey: routerDefaultKey},
			"alt-provider":     {URL: routerAlternateURL, APIKey: routerAlternateKey},
			"alt-alias":        {URL: routerAlternateURL, APIKey: routerAlternateKey},
			"other-key":        {URL: routerAlternateURL, APIKey: routerSecondKey},
			"local-provider":   {URL: routerLocalURL},
		},
	}
}

func routerTestDefaultClient() *routerTestChatter {
	return &routerTestChatter{label: "startup-default"}
}

func requireDegradations(t *testing.T, got []LinkDegradation, want ...LinkDegradation) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("degradations = %#v, want %#v", got, want)
	}
}

func requireTarget(t *testing.T, got Target, model, provider, baseURL string, client llm.Chatter) {
	t.Helper()
	if got.Model != model {
		t.Fatalf("model = %q, want %q", got.Model, model)
	}
	if got.Provider != provider {
		t.Fatalf("provider = %q, want %q", got.Provider, provider)
	}
	if got.BillingBaseURL != baseURL {
		t.Fatalf("billing base URL = %q, want %q", got.BillingBaseURL, baseURL)
	}
	if got.Client != client {
		t.Fatalf("client = %#v, want %#v", got.Client, client)
	}
}

func TestModelRouterPrecedence(t *testing.T) {
	ctx := context.Background()

	t.Run("pin beats override and default", func(t *testing.T) {
		view := routerTestView()
		view.Overrides = map[string]map[string]ChannelOverride{
			"discord": {"55501": {Provider: "alt-provider", Model: "override-model"}},
		}
		store := newRouterPinStore()
		store.set("alice", "discord:55501", "local-provider", "pin-model")
		factory := newRouterClientFactory()
		router := NewModelRouter(view, routerTestDefaultClient(), factory.newClient, store)

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		if target.Client == nil {
			t.Fatal("pin target has no client")
		}
		requireTarget(t, target, "pin-model", "local-provider", routerLocalURL, factory.call(t, 0).client)
		requireDegradations(t, degradations)
	})

	t.Run("override beats default", func(t *testing.T) {
		view := routerTestView()
		view.Overrides = map[string]map[string]ChannelOverride{
			"discord": {"55501": {Provider: "alt-provider", Model: "override-model"}},
		}
		router := NewModelRouter(view, routerTestDefaultClient(), newRouterClientFactory().newClient, newRouterPinStore())

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		if target.Model != "override-model" || target.Provider != "alt-provider" || target.BillingBaseURL != routerAlternateURL {
			t.Fatalf("override target = %#v", target)
		}
		if target.Client == nil {
			t.Fatal("override target has no client")
		}
		requireDegradations(t, degradations)
	})

	t.Run("default when nothing matches", func(t *testing.T) {
		factory := newRouterClientFactory()
		router := NewModelRouter(routerTestView(), routerTestDefaultClient(), factory.newClient, newRouterPinStore())

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		if target.Model != "default-model" || target.Provider != "default-provider" || target.BillingBaseURL != routerDefaultURL {
			t.Fatalf("default target = %#v", target)
		}
		if target.Client == nil {
			t.Fatal("default target has no client")
		}
		requireDegradations(t, degradations)
		if factory.callCount() != 0 {
			t.Fatalf("default link constructed %d clients, want 0", factory.callCount())
		}
	})
}

func TestModelRouterDefaultReusesStartupClient(t *testing.T) {
	defaultClient := routerTestDefaultClient()
	factory := newRouterClientFactory()
	router := NewModelRouter(routerTestView(), defaultClient, factory.newClient, newRouterPinStore())

	target, degradations := router.Resolve(context.Background(), "alice", "discord:55501", "discord", "55501")

	requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
	requireDegradations(t, degradations)
	if factory.callCount() != 0 {
		t.Fatalf("constructor calls = %d, want 0", factory.callCount())
	}
}

func TestModelRouterConstructedClientCache(t *testing.T) {
	ctx := context.Background()
	store := newRouterPinStore()
	store.set("alice", "discord:55501", "alt-provider", "alt-model")
	factory := newRouterClientFactory()
	router := NewModelRouter(routerTestView(), routerTestDefaultClient(), factory.newClient, store)

	first, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
	requireDegradations(t, degradations)
	second, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
	requireDegradations(t, degradations)

	if first.Client == nil || first.Client != second.Client {
		t.Fatalf("cache miss: first=%#v second=%#v", first.Client, second.Client)
	}
	if factory.callCount() != 1 {
		t.Fatalf("constructor calls = %d, want 1", factory.callCount())
	}
	call := factory.call(t, 0)
	if call.url != routerAlternateURL || call.apiKey != routerAlternateKey || call.virtualKey != routerVirtualKey {
		t.Fatalf("constructor call = %#v", call)
	}

	// A different key for the same URL is a distinct transport.
	store.set("alice", "discord:55501", "other-key", "other-model")
	third, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
	requireDegradations(t, degradations)
	if third.Client == first.Client {
		t.Fatal("different key reused the cached client")
	}
	if factory.callCount() != 2 {
		t.Fatalf("constructor calls = %d, want 2", factory.callCount())
	}
	if call := factory.call(t, 1); call.apiKey != routerSecondKey {
		t.Fatalf("second constructor call key = %q", call.apiKey)
	}

	// A provider alias sharing URL and key resolves to the same instance.
	store.set("alice", "discord:55501", "alt-alias", "alias-model")
	aliased, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
	requireDegradations(t, degradations)
	if aliased.Client != first.Client {
		t.Fatal("url+key alias did not reuse the cached client")
	}
	if factory.callCount() != 2 {
		t.Fatalf("constructor calls = %d, want 2", factory.callCount())
	}
}

func TestModelRouterPinOnDefaultTransport(t *testing.T) {
	store := newRouterPinStore()
	store.set("alice", "discord:55501", "default-provider", "pinned-default-model")
	factory := newRouterClientFactory()
	defaultClient := routerTestDefaultClient()
	router := NewModelRouter(routerTestView(), defaultClient, factory.newClient, store)

	target, degradations := router.Resolve(context.Background(), "alice", "discord:55501", "discord", "55501")

	requireTarget(t, target, "pinned-default-model", "default-provider", routerDefaultURL, defaultClient)
	requireDegradations(t, degradations)
	if factory.callCount() != 0 {
		t.Fatalf("constructor calls = %d, want 0", factory.callCount())
	}
}

func TestModelRouterKeylessEndpoint(t *testing.T) {
	ctx := context.Background()
	store := newRouterPinStore()
	store.set("alice", "discord:55501", "local-provider", "local-model")
	factory := newRouterClientFactory()
	router := NewModelRouter(routerTestView(), routerTestDefaultClient(), factory.newClient, store)

	first, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
	requireDegradations(t, degradations)
	if first.Client == nil {
		t.Fatal("keyless endpoint produced no client")
	}
	if first.BillingBaseURL != routerLocalURL {
		t.Fatalf("billing base URL = %q, want %q", first.BillingBaseURL, routerLocalURL)
	}

	second, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
	requireDegradations(t, degradations)
	if second.Client != first.Client {
		t.Fatal("keyless endpoint was not cached")
	}
	if factory.callCount() != 1 {
		t.Fatalf("constructor calls = %d, want 1", factory.callCount())
	}
	call := factory.call(t, 0)
	if call.url != routerLocalURL || call.apiKey != "" {
		t.Fatalf("keyless constructor call = %#v", call)
	}
}

func TestModelRouterUnreadablePin(t *testing.T) {
	ctx := context.Background()

	t.Run("falls through to override", func(t *testing.T) {
		view := routerTestView()
		view.Overrides = map[string]map[string]ChannelOverride{
			"discord": {"55501": {Provider: "alt-provider", Model: "override-model"}},
		}
		store := newRouterPinStore()
		store.fail(routerUnreadablePinError{cause: context.Canceled})
		router := NewModelRouter(view, routerTestDefaultClient(), newRouterClientFactory().newClient, store)

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		if target.Model != "override-model" {
			t.Fatalf("target model = %q, want override", target.Model)
		}
		requireDegradations(t, degradations, LinkDegradation{Link: "pin", Reason: "unreadable"})
	})

	t.Run("falls through to default", func(t *testing.T) {
		defaultClient := routerTestDefaultClient()
		store := newRouterPinStore()
		store.fail(routerUnreadablePinError{})
		router := NewModelRouter(routerTestView(), defaultClient, newRouterClientFactory().newClient, store)

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
		requireDegradations(t, degradations, LinkDegradation{Link: "pin", Reason: "unreadable"})
	})
}

func TestModelRouterStoreUnavailable(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "unavailable", err: errors.New("profile store is unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defaultClient := routerTestDefaultClient()
			store := newRouterPinStore()
			store.fail(tc.err)
			router := NewModelRouter(routerTestView(), defaultClient, newRouterClientFactory().newClient, store)

			target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

			requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
			requireDegradations(t, degradations, LinkDegradation{Link: "pin", Reason: "store_unavailable"})
		})
	}

	t.Run("nil store", func(t *testing.T) {
		defaultClient := routerTestDefaultClient()
		router := NewModelRouter(routerTestView(), defaultClient, newRouterClientFactory().newClient, nil)

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
		requireDegradations(t, degradations, LinkDegradation{Link: "pin", Reason: "store_unavailable"})
	})
}

func TestModelRouterPinDegradations(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown provider falls through to default", func(t *testing.T) {
		store := newRouterPinStore()
		store.set("alice", "discord:55501", "missing-provider", "ghost-model")
		defaultClient := routerTestDefaultClient()
		router := NewModelRouter(routerTestView(), defaultClient, newRouterClientFactory().newClient, store)

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
		requireDegradations(t, degradations, LinkDegradation{Link: "pin", Reason: "unknown_provider"})
	})

	t.Run("unknown provider falls through to override", func(t *testing.T) {
		view := routerTestView()
		view.Overrides = map[string]map[string]ChannelOverride{
			"discord": {"55501": {Provider: "alt-provider", Model: "override-model"}},
		}
		store := newRouterPinStore()
		store.set("alice", "discord:55501", "missing-provider", "ghost-model")
		router := NewModelRouter(view, routerTestDefaultClient(), newRouterClientFactory().newClient, store)

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		if target.Model != "override-model" {
			t.Fatalf("target model = %q, want override", target.Model)
		}
		requireDegradations(t, degradations, LinkDegradation{Link: "pin", Reason: "unknown_provider"})
	})

	t.Run("empty model", func(t *testing.T) {
		store := newRouterPinStore()
		store.set("alice", "discord:55501", "alt-provider", "")
		defaultClient := routerTestDefaultClient()
		router := NewModelRouter(routerTestView(), defaultClient, newRouterClientFactory().newClient, store)

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
		requireDegradations(t, degradations, LinkDegradation{Link: "pin", Reason: "empty_model"})
	})
}

func TestModelRouterOverrideDegradations(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown provider falls through to default", func(t *testing.T) {
		view := routerTestView()
		view.Overrides = map[string]map[string]ChannelOverride{
			"discord": {"55501": {Provider: "missing-provider", Model: "ghost-model"}},
		}
		defaultClient := routerTestDefaultClient()
		router := NewModelRouter(view, defaultClient, newRouterClientFactory().newClient, newRouterPinStore())

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
		requireDegradations(t, degradations, LinkDegradation{Link: "override", Reason: "unknown_provider"})
	})

	t.Run("empty model falls through to default", func(t *testing.T) {
		view := routerTestView()
		view.Overrides = map[string]map[string]ChannelOverride{
			"discord": {"55501": {Provider: "alt-provider"}},
		}
		defaultClient := routerTestDefaultClient()
		router := NewModelRouter(view, defaultClient, newRouterClientFactory().newClient, newRouterPinStore())

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
		requireDegradations(t, degradations, LinkDegradation{Link: "override", Reason: "empty_model"})
	})
}

func TestModelRouterClientUnavailable(t *testing.T) {
	ctx := context.Background()

	t.Run("pin without constructor falls through to default", func(t *testing.T) {
		store := newRouterPinStore()
		store.set("alice", "discord:55501", "alt-provider", "alt-model")
		defaultClient := routerTestDefaultClient()
		router := NewModelRouter(routerTestView(), defaultClient, nil, store)

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
		requireDegradations(t, degradations, LinkDegradation{Link: "pin", Reason: "client_unavailable"})
	})

	t.Run("constructor failure falls through to override", func(t *testing.T) {
		view := routerTestView()
		view.Endpoints["broken-provider"] = config.ProviderEndpoint{URL: "https://broken.example.com/v1", APIKey: "fake-broken-key"}
		view.Overrides = map[string]map[string]ChannelOverride{
			"discord": {"55501": {Provider: "alt-provider", Model: "override-model"}},
		}
		store := newRouterPinStore()
		store.set("alice", "discord:55501", "broken-provider", "broken-model")
		factory := newRouterClientFactory()
		factory.failURLs = map[string]bool{"https://broken.example.com/v1": true}
		router := NewModelRouter(view, routerTestDefaultClient(), factory.newClient, store)

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		if target.Model != "override-model" || target.Provider != "alt-provider" || target.BillingBaseURL != routerAlternateURL {
			t.Fatalf("fallthrough target = %#v", target)
		}
		requireTarget(t, target, "override-model", "alt-provider", routerAlternateURL, factory.call(t, 1).client)
		requireDegradations(t, degradations, LinkDegradation{Link: "pin", Reason: "client_unavailable"})
	})

	t.Run("override without constructor falls through to default", func(t *testing.T) {
		view := routerTestView()
		view.Overrides = map[string]map[string]ChannelOverride{
			"discord": {"55501": {Provider: "alt-provider", Model: "override-model"}},
		}
		defaultClient := routerTestDefaultClient()
		router := NewModelRouter(view, defaultClient, nil, newRouterPinStore())

		target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")

		requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
		requireDegradations(t, degradations, LinkDegradation{Link: "override", Reason: "client_unavailable"})
	})
}

func TestModelRouterSwapViewRotation(t *testing.T) {
	ctx := context.Background()
	view := routerTestView()
	view.Overrides = map[string]map[string]ChannelOverride{
		"discord": {"55501": {Provider: "alt-provider", Model: "before-model"}},
	}
	defaultClient := routerTestDefaultClient()
	factory := newRouterClientFactory()
	router := NewModelRouter(view, defaultClient, factory.newClient, newRouterPinStore())

	unchangedDefault, degradations := router.Resolve(ctx, "alice", "discord:55502", "discord", "55502")
	requireDegradations(t, degradations)
	requireTarget(t, unchangedDefault, "default-model", "default-provider", routerDefaultURL, defaultClient)

	before, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
	requireDegradations(t, degradations)
	if before.Model != "before-model" || before.BillingBaseURL != routerAlternateURL {
		t.Fatalf("before target = %#v", before)
	}

	rotated := routerTestView()
	rotated.Overrides = map[string]map[string]ChannelOverride{
		"discord": {"55501": {Provider: "local-provider", Model: "after-model"}},
	}
	router.SwapView(rotated)

	after, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
	requireDegradations(t, degradations)
	if after.Model != "after-model" || after.BillingBaseURL != routerLocalURL {
		t.Fatalf("after target = %#v", after)
	}

	// The earlier target is a value snapshot and keeps its routing.
	if before.Model != "before-model" || before.Provider != "alt-provider" || before.BillingBaseURL != routerAlternateURL {
		t.Fatalf("in-flight target mutated by swap: %#v", before)
	}

	// A rotated default is picked up for model/provider/URL; the startup
	// client stays bound as the default transport.
	rotatedDefault := routerTestView()
	rotatedDefault.Default = DefaultTarget{
		Model:      "rotated-model",
		Provider:   "rotated-provider",
		URL:        routerRotatedURL,
		APIKey:     routerRotatedKey,
		VirtualKey: routerVirtualKey,
	}
	rotatedDefault.Endpoints["rotated-provider"] = config.ProviderEndpoint{URL: routerRotatedURL, APIKey: routerRotatedKey}
	router.SwapView(rotatedDefault)
	callsBefore := factory.callCount()
	rotatedTarget, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
	requireDegradations(t, degradations)
	if factory.callCount() != callsBefore+1 {
		t.Fatalf("rotated default constructions = %d, want %d", factory.callCount(), callsBefore+1)
	}
	rotatedCall := factory.call(t, callsBefore)
	if rotatedCall.url != routerRotatedURL || rotatedCall.apiKey != routerRotatedKey || rotatedCall.virtualKey != routerVirtualKey {
		t.Fatalf("rotated default constructor call = %#v", rotatedCall)
	}
	requireTarget(t, rotatedTarget, "rotated-model", "rotated-provider", routerRotatedURL, rotatedCall.client)
	if rotatedTarget.Client == defaultClient {
		t.Fatal("rotated default reused the startup client")
	}

	// The rotated transport is cached like any other endpoint.
	cachedTarget, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
	requireDegradations(t, degradations)
	if cachedTarget.Client != rotatedTarget.Client || factory.callCount() != callsBefore+1 {
		t.Fatalf("rotated default was not cached: client=%#v calls=%d", cachedTarget.Client, factory.callCount())
	}
}

func TestModelRouterMissingOverrideIsSilent(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		overrides map[string]map[string]ChannelOverride
		gateway   string
		chatID    string
	}{
		{
			name:      "gateway absent",
			overrides: map[string]map[string]ChannelOverride{"discord": {"55501": {Provider: "alt-provider", Model: "override-model"}}},
			gateway:   "cron",
			chatID:    "job-55501",
		},
		{
			name:      "chat id absent",
			overrides: map[string]map[string]ChannelOverride{"discord": {"55501": {Provider: "alt-provider", Model: "override-model"}}},
			gateway:   "discord",
			chatID:    "55502",
		},
		{
			name:      "nil overrides",
			overrides: nil,
			gateway:   "discord",
			chatID:    "55501",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := routerTestView()
			view.Overrides = tc.overrides
			defaultClient := routerTestDefaultClient()
			router := NewModelRouter(view, defaultClient, newRouterClientFactory().newClient, newRouterPinStore())

			target, degradations := router.Resolve(ctx, "alice", "discord:55501", tc.gateway, tc.chatID)

			requireTarget(t, target, "default-model", "default-provider", routerDefaultURL, defaultClient)
			requireDegradations(t, degradations)
		})
	}
}

func TestModelRouterConcurrentSwapAndResolve(t *testing.T) {
	ctx := context.Background()

	first := routerTestView()
	first.Overrides = map[string]map[string]ChannelOverride{
		"discord": {"55501": {Provider: "alt-provider", Model: "first-model"}},
	}
	second := routerTestView()
	second.Overrides = map[string]map[string]ChannelOverride{
		"discord": {"55501": {Provider: "local-provider", Model: "second-model"}},
	}
	allowed := map[string]bool{
		"first-model|alt-provider|" + routerAlternateURL: true,
		"second-model|local-provider|" + routerLocalURL:  true,
	}

	router := NewModelRouter(first, routerTestDefaultClient(), newRouterClientFactory().newClient, newRouterPinStore())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			if i%2 == 0 {
				router.SwapView(first)
			} else {
				router.SwapView(second)
			}
		}
	}()
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 250; i++ {
				target, degradations := router.Resolve(ctx, "alice", "discord:55501", "discord", "55501")
				if len(degradations) != 0 {
					t.Errorf("unexpected degradations: %#v", degradations)
					return
				}
				key := target.Model + "|" + target.Provider + "|" + target.BillingBaseURL
				if !allowed[key] {
					t.Errorf("torn target: %#v", target)
					return
				}
				if target.Client == nil {
					t.Error("target has no client")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestFixedTarget(t *testing.T) {
	client := &routerTestChatter{label: "fixed"}
	target := FixedTarget("fixed-model", "fixed-provider", "https://fixed.example.com/v1", client)

	if target.Model != "fixed-model" || target.Provider != "fixed-provider" ||
		target.BillingBaseURL != "https://fixed.example.com/v1" || target.Client != client {
		t.Fatalf("fixed target = %#v", target)
	}
}
