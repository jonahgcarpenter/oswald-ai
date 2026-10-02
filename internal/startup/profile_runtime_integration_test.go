package startup

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

type integrationResponder struct {
	response *agent.Response
	command  commands.Result
}

func (*integrationResponder) StartProcessing() (func(), error) { return func() {}, nil }
func (*integrationResponder) SendFallback(string) error        { return errors.New("unexpected fallback") }
func (r *integrationResponder) SendCommandResponse(result commands.Result) error {
	r.command = result
	return nil
}
func (r *integrationResponder) SendAgentResponse(response *agent.Response) error {
	r.response = response
	return nil
}
func (*integrationResponder) SendAgentError(string) error {
	return errors.New("unexpected agent error")
}
func (*integrationResponder) CancelAgentResponse() error {
	return errors.New("unexpected cancellation")
}

type integrationMemoryModel struct{}

func (integrationMemoryModel) Chat(_ context.Context, req llm.ChatRequest, _ func(llm.ChatMessage)) (*llm.ChatResponse, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "user" && last.Content == "save synthetic note" {
		return &llm.ChatResponse{Model: req.Model, Message: llm.ChatMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "note-write", Function: llm.ToolFunction{Name: "memory", Arguments: map[string]interface{}{"target": "memory", "action": "add", "content": "shared-profile-private-canary"}}}}}}, nil
	}
	var content strings.Builder
	for _, message := range req.Messages {
		content.WriteString(message.Content)
	}
	answer := "no saved note"
	if strings.Contains(content.String(), "shared-profile-private-canary") {
		answer = "saved note visible"
	}
	return &llm.ChatResponse{Model: req.Model, Message: llm.ChatMessage{Role: "assistant", Content: answer}}, nil
}

func TestProfileRuntimeSharesOnlyConfiguredMemoryAndRetainsSnapshotsAcrossRestart(t *testing.T) {
	cfg := profileStartupFixture(t)
	cfg.DiscordToken = "synthetic-token"
	cfg.DiscordPolicy = config.AdmissionPolicy{Mode: "allow"}
	cfg.BlueBubblesListenPort = "12346"
	cfg.BlueBubblesPolicy = config.AdmissionPolicy{Mode: "allow"}
	cfg.ProfileRoutes = []config.ProfileRoute{{Name: "discord", Platform: "discord", UserID: "123", Profile: "alice"}, {Name: "messages", Platform: "imessage", UserID: "+15551234567", Profile: "alice"}}
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(io.Discard)
	for phase := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		seams := profileDependencies{newClient: func(*config.Config, *config.Logger) llm.Chatter { return integrationMemoryModel{} }, newGateways: func(_ *config.Config, resolver identity.Resolver, deps gatewayruntime.Dependencies, _ *config.Logger) ([]gateway.Service, error) {
			return []gateway.Service{profileFakeGateway{start: func(*broker.Broker) error {
				defer cancel()
				run := func() error {
					discord, err := resolver.Resolve("discord", "123", true)
					if err != nil {
						return err
					}
					messages, err := resolver.Resolve("imessage", "+15551234567", true)
					if err != nil {
						return err
					}
					api, err := resolver.LocalOpenAIPrincipal(ctx)
					if err != nil {
						return err
					}
					if discord.CanonicalUserID != "alice" || messages.CanonicalUserID != "alice" || api.CanonicalUserID != "api" {
						return errors.New("configured ownership changed")
					}
					if _, err := resolver.Resolve("discord", "456", true); err == nil {
						return errors.New("unmapped identity admitted")
					}
					request := func(p identity.Principal, key, prompt string, stateless bool) (*integrationResponder, error) {
						r := &integrationResponder{}
						outcome := gatewayruntime.Execute(gatewayruntime.Request{Principal: p, SessionKey: key, Text: prompt, Stateless: stateless}, deps, r)
						return r, outcome.Err
					}
					if phase == 0 {
						if _, err := request(discord, "discord:dm:123", "before write", false); err != nil {
							return err
						}
						if _, err := request(messages, "imessage:dm:+15551234567", "save synthetic note", false); err != nil {
							return err
						}
					}
					frozen, err := request(discord, "discord:dm:123", "check frozen note", false)
					if err != nil {
						return err
					}
					if frozen.response == nil || frozen.response.Response != "no saved note" {
						return errors.New("frozen pair changed before reset")
					}
					fresh, err := request(messages, "imessage:fresh", "check shared note", false)
					if err != nil {
						return err
					}
					if fresh.response == nil || fresh.response.Response != "saved note visible" {
						return errors.New("configured identities do not share private files")
					}
					isolated, err := request(api, "openai:synthetic", "check isolated note", true)
					if err != nil {
						return err
					}
					if isolated.response == nil || isolated.response.Response != "no saved note" {
						return errors.New("another profile's notes leaked")
					}
					if phase == 1 {
						reset, err := request(discord, "discord:dm:123", "/reset", false)
						if err != nil {
							return err
						}
						if reset.command.Text == "" {
							return errors.New("reset response missing")
						}
						updated, err := request(discord, "discord:dm:123", "check reset note", false)
						if err != nil {
							return err
						}
						if updated.response == nil || updated.response.Response != "saved note visible" {
							return errors.New("reset lost file memory or retained old snapshot")
						}
					}
					return nil
				}
				done <- run()
				return nil
			}}}, nil
		}}
		if err := runProfilesWith(ctx, cfg, log, seams); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
