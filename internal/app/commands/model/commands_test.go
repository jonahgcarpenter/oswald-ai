package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

const testSession = "discord:dm:555000111"

type fakeRouter struct {
	target       agent.Target
	degradations []agent.LinkDegradation
	providers    []string
	statuses     []agent.ProviderStatus
	models       map[string][]llm.ModelEntry
	listErr      error

	resolveOwner, resolveSession, resolveGateway, resolveChat string
	listedProvider                                            string
}

func (f *fakeRouter) Resolve(_ context.Context, owner, sessionKey, gateway, chatID string) (agent.Target, []agent.LinkDegradation) {
	f.resolveOwner, f.resolveSession, f.resolveGateway, f.resolveChat = owner, sessionKey, gateway, chatID
	return f.target, f.degradations
}

func (f *fakeRouter) HasProvider(provider string) bool {
	for _, name := range f.providers {
		if name == provider {
			return true
		}
	}
	return false
}

func (f *fakeRouter) Providers() []string { return append([]string(nil), f.providers...) }

func (f *fakeRouter) ListProviders(context.Context) []agent.ProviderStatus { return f.statuses }

func (f *fakeRouter) ListModels(_ context.Context, provider string) ([]llm.ModelEntry, error) {
	f.listedProvider = provider
	if f.listErr != nil {
		return nil, f.listErr
	}
	models, ok := f.models[provider]
	if !ok {
		return nil, fmt.Errorf("%w: %q", agent.ErrProviderUnknown, provider)
	}
	return models, nil
}

type fakePins struct {
	provider   string
	model      string
	found      bool
	sessionErr error
	setErr     error

	gotOwner, gotSession, gotProvider, gotModel string
	setCalls                                    int
}

type fakeDefaults struct {
	setErr                error
	gotProvider, gotModel string
	setCalls              int
}

func (f *fakeDefaults) SetProfileDefault(_ context.Context, provider, model string) error {
	f.setCalls++
	f.gotProvider, f.gotModel = provider, model
	return f.setErr
}

func (f *fakePins) SessionModel(context.Context, string, string) (string, string, bool, error) {
	if f.sessionErr != nil {
		return "", "", false, f.sessionErr
	}
	return f.provider, f.model, f.found, nil
}

func (f *fakePins) SetSessionModel(_ context.Context, owner, sessionKey, provider, model string) error {
	f.setCalls++
	f.gotOwner, f.gotSession, f.gotProvider, f.gotModel = owner, sessionKey, provider, model
	return f.setErr
}

func testPrincipal() identity.Principal {
	return identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "555000111", Assurance: identity.AssuranceDiscordGateway}
}

func testRequest(args ...string) commands.Request {
	return commands.Request{
		Principal:  testPrincipal(),
		ChatID:     "555000111",
		SessionKey: testSession,
		Raw:        strings.TrimSpace("/model " + strings.Join(args, " ")),
		Args:       append([]string(nil), args...),
	}
}

func TestShowReportsConversationPinAndProviders(t *testing.T) {
	router := &fakeRouter{
		target: agent.Target{Provider: "example", Model: "model-a"},
		statuses: []agent.ProviderStatus{
			{Name: "example", Available: true, ModelCount: 12},
			{Name: "other", Available: false},
		},
	}
	pins := &fakePins{provider: "example", model: "model-a", found: true}

	result, err := New(router, pins, &fakeDefaults{}).Execute(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := "Current model: example/model-a (conversation pin)\n" +
		"Providers:\n" +
		"  example — 12 models\n" +
		"  other — unavailable\n" +
		"Use /model <provider> to list its models, /model <provider>/<model> to set."
	if result.Text != want {
		t.Fatalf("text = %q, want %q", result.Text, want)
	}
	if result.Outcome.Status != "ok" || result.Outcome.Operation != "model.show" || result.Outcome.IsChanged {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
	if router.resolveOwner != "alice" || router.resolveSession != testSession || router.resolveGateway != "discord" || router.resolveChat != "555000111" {
		t.Fatalf("resolve scope = %q %q %q %q", router.resolveOwner, router.resolveSession, router.resolveGateway, router.resolveChat)
	}
}

func TestShowFallsBackToProfileDefaultProvenance(t *testing.T) {
	tests := []struct {
		name string
		pins *fakePins
	}{
		{name: "pin unequal to resolved target", pins: &fakePins{provider: "example", model: "model-b", found: true}},
		{name: "no pin", pins: &fakePins{}},
		{name: "pin read error", pins: &fakePins{sessionErr: errors.New("state read failed")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := &fakeRouter{target: agent.Target{Provider: "example", Model: "model-a"}}
			result, err := New(router, test.pins, &fakeDefaults{}).Execute(context.Background(), testRequest())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(result.Text, "(profile default)") || strings.Contains(result.Text, "(conversation pin)") {
				t.Fatalf("text = %q", result.Text)
			}
		})
	}
}

func TestShowWithoutConfiguredProviders(t *testing.T) {
	router := &fakeRouter{target: agent.Target{Provider: "example", Model: "model-a"}}
	result, err := New(router, &fakePins{}, &fakeDefaults{}).Execute(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := "Current model: example/model-a (profile default)\n" +
		"Providers: none configured\n" +
		"Use /model <provider> to list its models, /model <provider>/<model> to set."
	if result.Text != want {
		t.Fatalf("text = %q, want %q", result.Text, want)
	}
}

func TestListRendersContextWindows(t *testing.T) {
	router := &fakeRouter{
		providers: []string{"example"},
		models: map[string][]llm.ModelEntry{"example": {
			{ID: "model-a", Window: 128000},
			{ID: "model-b"},
		}},
	}
	result, err := New(router, &fakePins{}, &fakeDefaults{}).Execute(context.Background(), testRequest("example"))
	if err != nil {
		t.Fatal(err)
	}
	want := "  model-a (context 128000)\n  model-b"
	if result.Text != want {
		t.Fatalf("text = %q, want %q", result.Text, want)
	}
	if result.Outcome.Status != "ok" || result.Outcome.Operation != "model.list" || result.Outcome.IsChanged {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
	if router.listedProvider != "example" {
		t.Fatalf("listed provider = %q", router.listedProvider)
	}
}

func TestListCapsAndNotesTruncation(t *testing.T) {
	entries := func(count int) []llm.ModelEntry {
		models := make([]llm.ModelEntry, 0, count)
		for i := 0; i < count; i++ {
			models = append(models, llm.ModelEntry{ID: fmt.Sprintf("model-%02d", i)})
		}
		return models
	}

	t.Run("exactly forty is complete", func(t *testing.T) {
		router := &fakeRouter{providers: []string{"example"}, models: map[string][]llm.ModelEntry{"example": entries(40)}}
		result, err := New(router, &fakePins{}, &fakeDefaults{}).Execute(context.Background(), testRequest("example"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Text, "model-39") || strings.Contains(result.Text, "…and") {
			t.Fatalf("text = %q", result.Text)
		}
	})

	t.Run("forty-one truncates", func(t *testing.T) {
		router := &fakeRouter{providers: []string{"example"}, models: map[string][]llm.ModelEntry{"example": entries(41)}}
		result, err := New(router, &fakePins{}, &fakeDefaults{}).Execute(context.Background(), testRequest("example"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Text, "model-39") || strings.Contains(result.Text, "model-40") {
			t.Fatalf("text = %q", result.Text)
		}
		if !strings.HasSuffix(result.Text, "  …and 1 more — oswald model list shows all") {
			t.Fatalf("missing truncation note: %q", result.Text)
		}
	})
}

func TestListUnknownProviderNamesConfiguredProviders(t *testing.T) {
	router := &fakeRouter{providers: []string{"example", "other"}}
	result, err := New(router, &fakePins{}, &fakeDefaults{}).Execute(context.Background(), testRequest("bogus"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Unknown provider: bogus. Configured: example, other." {
		t.Fatalf("text = %q", result.Text)
	}
	if result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "unknown_provider" || result.Outcome.Operation != "model.list" {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
}

func TestListFetchErrorHidesRawError(t *testing.T) {
	canary := "https://secret.example.com/v1/models?key=canary-key"
	router := &fakeRouter{
		providers: []string{"example"},
		models:    map[string][]llm.ModelEntry{"example": {{ID: "model-a"}}},
		listErr:   errors.New("models list request failed: " + canary),
	}
	result, err := New(router, &fakePins{}, &fakeDefaults{}).Execute(context.Background(), testRequest("example"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Models for example are unavailable right now." {
		t.Fatalf("text = %q", result.Text)
	}
	for _, leak := range []string{"canary", "secret", "https", "models list"} {
		if strings.Contains(result.Text, leak) {
			t.Fatalf("raw error leaked %q: %q", leak, result.Text)
		}
	}
	if result.Outcome.Status != "error" || result.Outcome.ReasonCode != "provider_unavailable" || result.Outcome.Operation != "model.list" {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
}

func TestSetUnknownProvider(t *testing.T) {
	router := &fakeRouter{providers: []string{"example"}}
	pins := &fakePins{}
	result, err := New(router, pins, &fakeDefaults{}).Execute(context.Background(), testRequest("bogus/model-a"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Unknown provider: bogus. Configured: example." {
		t.Fatalf("text = %q", result.Text)
	}
	if result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "unknown_provider" || result.Outcome.Operation != "model.set" {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
	if pins.setCalls != 0 {
		t.Fatal("unknown provider was persisted")
	}
}

func TestSetNormalizesCustomPrefixAndKeepsModelSlashes(t *testing.T) {
	router := &fakeRouter{providers: []string{"openrouter"}}
	pins := &fakePins{}
	defaults := &fakeDefaults{}
	result, err := New(router, pins, defaults).Execute(context.Background(), testRequest("custom:openrouter/openai/gpt-4o"))
	if err != nil {
		t.Fatal(err)
	}
	if pins.setCalls != 1 || pins.gotProvider != "openrouter" || pins.gotModel != "openai/gpt-4o" {
		t.Fatalf("pin write = calls=%d provider=%q model=%q", pins.setCalls, pins.gotProvider, pins.gotModel)
	}
	if defaults.setCalls != 0 {
		t.Fatal("conversation pin wrote the profile default")
	}
	if pins.gotOwner != "alice" || pins.gotSession != testSession {
		t.Fatalf("pin scope = owner=%q session=%q", pins.gotOwner, pins.gotSession)
	}
	if !strings.HasPrefix(result.Text, "Model set to openrouter/openai/gpt-4o for this conversation.") {
		t.Fatalf("text = %q", result.Text)
	}
	if result.Outcome.Status != "ok" || result.Outcome.Operation != "model.set" || !result.Outcome.IsChanged || result.Outcome.AffectedCount != 1 {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
}

func TestSetTrailingSlashListsProvider(t *testing.T) {
	router := &fakeRouter{
		providers: []string{"example"},
		models:    map[string][]llm.ModelEntry{"example": {{ID: "model-a", Window: 8000}}},
	}
	pins := &fakePins{}
	result, err := New(router, pins, &fakeDefaults{}).Execute(context.Background(), testRequest("example/"))
	if err != nil {
		t.Fatal(err)
	}
	if pins.setCalls != 0 {
		t.Fatal("empty model was persisted")
	}
	if router.listedProvider != "example" || result.Text != "  model-a (context 8000)" {
		t.Fatalf("listed provider = %q text = %q", router.listedProvider, result.Text)
	}
	if result.Outcome.Operation != "model.list" {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
}

func TestSetRejectsInvalidModelNames(t *testing.T) {
	router := &fakeRouter{providers: []string{"example"}}
	tests := []struct {
		name  string
		model string
	}{
		{name: "oversize", model: strings.Repeat("m", modelRuneLimit+1)},
		{name: "invalid UTF-8", model: "bad\xff"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pins := &fakePins{}
			result, err := New(router, pins, &fakeDefaults{}).Execute(context.Background(), testRequest("example/"+test.model))
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "invalid_arguments" {
				t.Fatalf("outcome = %+v", result.Outcome)
			}
			if !strings.Contains(result.Text, "/model <provider>/<model>") || pins.setCalls != 0 {
				t.Fatalf("text = %q calls = %d", result.Text, pins.setCalls)
			}
		})
	}
}

func TestSetAcceptsModelAtRuneLimit(t *testing.T) {
	router := &fakeRouter{providers: []string{"example"}}
	pins := &fakePins{}
	result, err := New(router, pins, &fakeDefaults{}).Execute(context.Background(), testRequest("example/"+strings.Repeat("m", modelRuneLimit)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != "ok" || pins.setCalls != 1 {
		t.Fatalf("outcome = %+v calls = %d", result.Outcome, pins.setCalls)
	}
}

func TestSetStoreErrorHidesRawError(t *testing.T) {
	router := &fakeRouter{providers: []string{"example"}}
	pins := &fakePins{setErr: errors.New("write state_meta failed: canary-raw")}
	result, err := New(router, pins, &fakeDefaults{}).Execute(context.Background(), testRequest("example/model-a"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Could not set the model for this conversation." {
		t.Fatalf("text = %q", result.Text)
	}
	if strings.Contains(result.Text, "canary") || strings.Contains(result.Text, "state_meta") {
		t.Fatalf("raw store error leaked: %q", result.Text)
	}
	if result.Outcome.Status != "error" || result.Outcome.ReasonCode != "operation_failed" || result.Outcome.Operation != "model.set" || result.Outcome.IsChanged {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
}

func TestSetGlobalWritesDefaultAndPinsConversation(t *testing.T) {
	router := &fakeRouter{providers: []string{"example"}}
	pins := &fakePins{}
	defaults := &fakeDefaults{}
	result, err := New(router, pins, defaults).Execute(context.Background(), testRequest("custom:example/model-a", "--global"))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.setCalls != 1 || defaults.gotProvider != "example" || defaults.gotModel != "model-a" {
		t.Fatalf("default write = calls=%d provider=%q model=%q", defaults.setCalls, defaults.gotProvider, defaults.gotModel)
	}
	if pins.setCalls != 1 || pins.gotProvider != "example" || pins.gotModel != "model-a" || pins.gotOwner != "alice" || pins.gotSession != testSession {
		t.Fatalf("pin write = calls=%d provider=%q model=%q owner=%q session=%q", pins.setCalls, pins.gotProvider, pins.gotModel, pins.gotOwner, pins.gotSession)
	}
	want := "Profile default set to example/model-a — applies live (the running server picks it up within seconds). This conversation is pinned to it."
	if result.Text != want {
		t.Fatalf("text = %q, want %q", result.Text, want)
	}
	if result.Outcome.Status != "ok" || result.Outcome.Operation != "model.set_global" || !result.Outcome.IsChanged || result.Outcome.AffectedCount != 2 {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
}

func TestSetGlobalUnknownProviderRejectedBeforeWrite(t *testing.T) {
	router := &fakeRouter{providers: []string{"example"}}
	pins := &fakePins{}
	defaults := &fakeDefaults{}
	result, err := New(router, pins, defaults).Execute(context.Background(), testRequest("bogus/model-a", "--global"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Unknown provider: bogus. Configured: example." {
		t.Fatalf("text = %q", result.Text)
	}
	if result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "unknown_provider" || result.Outcome.Operation != "model.set_global" {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
	if defaults.setCalls != 0 || pins.setCalls != 0 {
		t.Fatal("unknown provider reached the write path")
	}
}

func TestSetGlobalRejectsInvalidModelNames(t *testing.T) {
	router := &fakeRouter{providers: []string{"example"}}
	tests := []struct {
		name  string
		model string
	}{
		{name: "oversize", model: strings.Repeat("m", modelRuneLimit+1)},
		{name: "invalid UTF-8", model: "bad\xff"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pins := &fakePins{}
			defaults := &fakeDefaults{}
			result, err := New(router, pins, defaults).Execute(context.Background(), testRequest("example/"+test.model, "--global"))
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "invalid_arguments" || result.Outcome.Operation != "model.set_global" {
				t.Fatalf("outcome = %+v", result.Outcome)
			}
			if !strings.Contains(result.Text, "/model <provider>/<model>") || defaults.setCalls != 0 || pins.setCalls != 0 {
				t.Fatalf("text = %q defaults = %d pins = %d", result.Text, defaults.setCalls, pins.setCalls)
			}
		})
	}
}

func TestSetGlobalWriteErrorHidesRawError(t *testing.T) {
	router := &fakeRouter{providers: []string{"example"}}
	pins := &fakePins{}
	defaults := &fakeDefaults{setErr: errors.New("config write failed: canary-provider-secret")}
	result, err := New(router, pins, defaults).Execute(context.Background(), testRequest("example/model-a", "--global"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Could not set the profile default." {
		t.Fatalf("text = %q", result.Text)
	}
	if strings.Contains(result.Text, "canary") || strings.Contains(result.Text, "config write") {
		t.Fatalf("raw config error leaked: %q", result.Text)
	}
	if result.Outcome.Status != "error" || result.Outcome.ReasonCode != "operation_failed" || result.Outcome.Operation != "model.set_global" || result.Outcome.IsChanged {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
	if pins.setCalls != 0 {
		t.Fatal("pin attempted after failed config write")
	}
}

func TestSetGlobalPinFailureKeepsCommittedDefault(t *testing.T) {
	router := &fakeRouter{providers: []string{"example"}}
	pins := &fakePins{setErr: errors.New("state write failed: canary-raw")}
	defaults := &fakeDefaults{}
	result, err := New(router, pins, defaults).Execute(context.Background(), testRequest("example/model-a", "--global"))
	if err != nil {
		t.Fatal(err)
	}
	want := "Profile default set to example/model-a — applies live. Could not pin this conversation."
	if result.Text != want {
		t.Fatalf("text = %q, want %q", result.Text, want)
	}
	if strings.Contains(result.Text, "canary") {
		t.Fatalf("raw pin error leaked: %q", result.Text)
	}
	if result.Outcome.Status != "ok" || result.Outcome.Operation != "model.set_global" || !result.Outcome.IsChanged || result.Outcome.AffectedCount != 1 {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
	if defaults.setCalls != 1 {
		t.Fatalf("default write calls = %d", defaults.setCalls)
	}
}

func TestGlobalFlagRejectedOutsideSetGlobalShape(t *testing.T) {
	positions := [][]string{
		{"--global"},
		{"--global", "example/model-a"},
		{"example/model-a", "--global", "extra"},
		{"example", "--global"},
		{"example/", "--global"},
	}
	for _, args := range positions {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			router := &fakeRouter{providers: []string{"example"}}
			pins := &fakePins{}
			defaults := &fakeDefaults{}
			result, err := New(router, pins, defaults).Execute(context.Background(), testRequest(args...))
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "invalid_arguments" || result.Outcome.Operation != "" {
				t.Fatalf("outcome = %+v", result.Outcome)
			}
			if !strings.Contains(result.Text, "Use: /model [provider]") {
				t.Fatalf("text = %q", result.Text)
			}
			if defaults.setCalls != 0 || pins.setCalls != 0 {
				t.Fatal("malformed global request did work")
			}
		})
	}
}

func TestResolveFenceTargetsOnlyForSetGlobal(t *testing.T) {
	handler := New(&fakeRouter{}, &fakePins{}, &fakeDefaults{})
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: handler})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		raw  string
		want []string
	}{
		{raw: "/model p/m --global", want: []string{"alice"}},
		{raw: "/model", want: nil},
		{raw: "/model p", want: nil},
		{raw: "/model p/m", want: nil},
		{raw: "/model --global", want: nil},
		{raw: "/model --global p/m", want: nil},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			got, err := service.ResolveFenceTargets(context.Background(), commands.Request{Principal: testPrincipal(), Raw: test.raw})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("fence targets = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResolveFenceTargetsNeverFencesUnauthenticated(t *testing.T) {
	handler := New(&fakeRouter{}, &fakePins{}, &fakeDefaults{})
	resolver, ok := handler.(commands.FenceTargetResolver)
	if !ok {
		t.Fatal("handler does not resolve fence targets")
	}
	got, err := resolver.ResolveFenceTargets(context.Background(), commands.Request{Raw: "/model p/m --global", Args: []string{"p/m", "--global"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unauthenticated request fenced %v", got)
	}
}

func TestTooManyArgumentsRejected(t *testing.T) {
	result, err := New(&fakeRouter{}, &fakePins{}, &fakeDefaults{}).Execute(context.Background(), testRequest("example", "extra"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "invalid_arguments" {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
	if !strings.Contains(result.Text, "Use: /model [provider]") {
		t.Fatalf("text = %q", result.Text)
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	result, err := New(&fakeRouter{}, &fakePins{}, &fakeDefaults{}).Execute(context.Background(), commands.Request{Raw: "/model"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != "rejected" || result.Outcome.ReasonCode != "authentication_required" {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
}

func TestServiceRegistrationAndDispatch(t *testing.T) {
	router := &fakeRouter{
		providers: []string{"example"},
		target:    agent.Target{Provider: "example", Model: "model-a"},
	}
	pins := &fakePins{}
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: New(router, pins, &fakeDefaults{})})
	if err != nil {
		t.Fatal(err)
	}
	definitions := service.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "model" {
		t.Fatalf("definitions = %+v", definitions)
	}
	if service.CanonicalName("model") != "model" {
		t.Fatalf("canonical name = %q", service.CanonicalName("model"))
	}

	result, err := service.Execute(context.Background(), commands.Request{
		Principal:  testPrincipal(),
		ChatID:     "555000111",
		SessionKey: testSession,
		Raw:        "/model example/model-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Status != "ok" || result.Outcome.Operation != "model.set" || !result.Outcome.IsChanged || result.Outcome.AffectedCount != 1 {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
	if pins.setCalls != 1 || pins.gotProvider != "example" || pins.gotModel != "model-a" {
		t.Fatalf("pin write = calls=%d provider=%q model=%q", pins.setCalls, pins.gotProvider, pins.gotModel)
	}
}

func TestServiceMapsUnconfiguredHandlerErrorToOutcome(t *testing.T) {
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: New(nil, nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(context.Background(), commands.Request{Principal: testPrincipal(), Raw: "/model"})
	if err == nil {
		t.Fatal("unconfigured handler succeeded")
	}
	if result.Outcome.Status != "error" || result.Outcome.ReasonCode != "operation_failed" {
		t.Fatalf("outcome = %+v", result.Outcome)
	}
}
