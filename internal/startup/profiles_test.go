package startup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

type profileFakeModel struct {
	mutex    sync.Mutex
	requests []llm.ChatRequest
}

func (model *profileFakeModel) Chat(_ context.Context, request llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	model.mutex.Lock()
	defer model.mutex.Unlock()
	model.requests = append(model.requests, request)
	return &llm.ChatResponse{Model: request.Model, Message: llm.ChatMessage{Role: "assistant", Content: "synthetic answer"}}, nil
}

type profileFakeGateway struct{ start func(*broker.Broker) error }

func (g profileFakeGateway) Name() string                 { return "test" }
func (g profileFakeGateway) Start(b *broker.Broker) error { return g.start(b) }

func profileStartupFixture(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SOUL.md"), []byte("synthetic default policy"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ProfileRoot: root, ProfileName: "default", Profiles: map[string]*config.Config{}, OpenAIListenPort: "12345", WorkerPoolSize: 1, LLMGatewayModel: "fake/default", LLMGatewayURL: "http://synthetic.invalid", ComfyUIGenerationTimeout: config.DefaultRetentionPolicy().MaintenanceInterval}
	for _, name := range []string{"alice", "api"} {
		path := filepath.Join(root, "profiles", name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		profile := *cfg
		profile.ProfileRoot, profile.ProfileName, profile.Profiles = path, name, nil
		profile.LLMGatewayModel = "fake/" + name
		cfg.Profiles[name] = &profile
	}
	return cfg
}

func TestProfileStartupUsesIndependentAgentsAndOnlyPublicCommands(t *testing.T) {
	cfg := profileStartupFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := &profileFakeModel{}
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(io.Discard)
	done := make(chan error, 1)
	seams := profileDependencies{newClient: func(*config.Config, *config.Logger) llm.Chatter { return model }, newGateways: func(_ *config.Config, resolver identity.Resolver, deps gatewayruntime.Dependencies, _ *config.Logger) ([]gateway.Service, error) {
		return []gateway.Service{profileFakeGateway{start: func(b *broker.Broker) error {
			principal, err := resolver.LocalOpenAIPrincipal(ctx)
			if err != nil {
				done <- err
				cancel()
				return nil
			}
			selected, ok := deps.ForProfile("api")
			if !ok {
				done <- errors.New("missing API runtime")
				cancel()
				return nil
			}
			definitions := selected.Commands.Definitions()
			if len(definitions) != 3 {
				done <- errors.New("unexpected profile commands")
				cancel()
				return nil
			}
			for _, definition := range definitions {
				if definition.Name != "help" && definition.Name != "reset" && definition.Name != "stop" {
					done <- errors.New("obsolete command registered")
					cancel()
					return nil
				}
			}
			responses := make(chan broker.Result, 1)
			err = selected.Broker.Submit(&broker.Request{Principal: principal, SessionKey: "openai:test", Stateless: true, Prompt: "synthetic prompt", ResponseChan: responses})
			if err == nil {
				response := <-responses
				err = response.Err
				if err == nil && response.Response == nil {
					err = errors.New("missing profile response")
				}
			}
			done <- err
			cancel()
			return nil
		}}}, nil
	}}
	err := runProfilesWith(ctx, cfg, log, seams)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	model.mutex.Lock()
	defer model.mutex.Unlock()
	if len(model.requests) != 1 || model.requests[0].Model != "fake/api" {
		t.Fatal("API selected another profile")
	}
	for _, root := range []string{cfg.ProfileRoot, cfg.Profiles["alice"].ProfileRoot, cfg.Profiles["api"].ProfileRoot} {
		if _, err := os.Stat(filepath.Join(root, "state.db")); err != nil {
			t.Fatal("missing profile-local state", err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.ProfileRoot, "database", "oswald.db")); !os.IsNotExist(err) {
		t.Fatal("shared legacy database created")
	}
}

func TestProfileStartupCleansUpBeforeReturningValidationFailure(t *testing.T) {
	var output bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&output)
	if err := Run(context.Background(), &config.Config{}, log, io.Discard); err == nil {
		t.Fatal("missing YAML configuration accepted")
	}
	if strings.Count(output.String(), `"event":"app.shutdown.complete"`) != 1 {
		t.Fatal("cleanup boundary missing")
	}
}

func TestProfileStartupPartialFailureClosesAcquiredStores(t *testing.T) {
	cfg := profileStartupFixture(t)
	var output bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&output)
	gatewayCalled := false
	err := runProfilesWith(context.Background(), cfg, log, profileDependencies{newClient: func(profile *config.Config, _ *config.Logger) llm.Chatter {
		if profile.ProfileName == "api" {
			return nil
		}
		return &profileFakeModel{}
	}, newGateways: func(*config.Config, identity.Resolver, gatewayruntime.Dependencies, *config.Logger) ([]gateway.Service, error) {
		gatewayCalled = true
		return nil, errors.New("must not start gateways")
	}})
	var failure *Error
	if !errors.As(err, &failure) || failure.Event != "app.session_compactor.init_failed" || gatewayCalled {
		t.Fatal("partial initialization failure was not fenced", err)
	}
	if strings.Count(output.String(), `"event":"app.shutdown.complete"`) != 1 {
		t.Fatal("partial cleanup boundary missing")
	}
	for _, name := range []string{"alice", "api"} {
		store, err := memory.NewProfileStore(context.Background(), cfg.Profiles[name].ProfileRoot, name, nil)
		if err != nil {
			t.Fatal("acquired store could not reopen", err)
		}
		store.Close()
	}
}
