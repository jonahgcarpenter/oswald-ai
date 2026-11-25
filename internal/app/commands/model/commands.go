// Package model implements the /model chat command family: showing the
// conversation's resolved model, listing a provider's models live, and pinning
// the conversation to one provider/model pair.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

const (
	// listEntryLimit bounds the provider listing rendered into one chat reply.
	listEntryLimit = 40
	// modelRuneLimit matches the persisted pin field bound.
	modelRuneLimit = 256
	// globalFlag is the not-yet-supported profile-default selector.
	globalFlag = "--global"
)

// Router is the model-routing surface the /model command needs.
type Router interface {
	Resolve(ctx context.Context, owner, sessionKey, gateway, chatID string) (agent.Target, []agent.LinkDegradation)
	HasProvider(provider string) bool
	Providers() []string
	ListProviders(ctx context.Context) []agent.ProviderStatus
	ListModels(ctx context.Context, provider string) ([]llm.ModelEntry, error)
}

// Pins reads and writes the conversation's model pin.
type Pins interface {
	SessionModel(ctx context.Context, owner, sessionKey string) (provider, model string, found bool, err error)
	SetSessionModel(ctx context.Context, owner, sessionKey, provider, model string) error
}

type handler struct {
	router Router
	pins   Pins
}

// New creates the /model command.
func New(router Router, pins Pins) commands.Handler {
	return &handler{router: router, pins: pins}
}

func (h *handler) Definition() commands.Definition {
	return commands.Definition{Name: "model", Summary: "Show or set the model for this conversation", Usage: "/model [provider] | /model <provider>/<model>"}
}

func (h *handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if !req.Principal.Authenticated() {
		return commands.Result{Text: "Model routing requires an authenticated identity.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "authentication_required"}}, nil
	}
	for _, arg := range req.Args {
		if arg == globalFlag {
			return commands.Result{Text: "Switching the profile default from chat is not available yet.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "not_available"}}, nil
		}
	}
	if len(req.Args) > 1 {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	if h.router == nil || h.pins == nil {
		return commands.Result{}, errors.New("model command is not configured")
	}
	if len(req.Args) == 0 {
		return h.show(ctx, req), nil
	}

	arg := strings.TrimSpace(req.Args[0])
	if !strings.Contains(arg, "/") {
		return h.list(ctx, arg), nil
	}
	provider, model, _ := strings.Cut(arg, "/")
	provider = normalizeProvider(provider)
	if model = strings.TrimSpace(model); model == "" {
		// A trailing slash selects the provider listing instead of a pin.
		return h.list(ctx, provider), nil
	}
	return h.set(ctx, req, provider, model), nil
}

// show renders the resolved target, its provenance, and every configured
// provider's listing availability.
func (h *handler) show(ctx context.Context, req commands.Request) commands.Result {
	owner := req.Principal.CanonicalUserID
	target, _ := h.router.Resolve(ctx, owner, req.SessionKey, req.Principal.Gateway, req.ChatID)

	provenance := "profile default"
	if provider, model, found, err := h.pins.SessionModel(ctx, owner, req.SessionKey); err == nil && found &&
		strings.TrimSpace(provider) == target.Provider && strings.TrimSpace(model) == target.Model {
		provenance = "conversation pin"
	}

	var text strings.Builder
	fmt.Fprintf(&text, "Current model: %s/%s (%s)\n", target.Provider, target.Model, provenance)
	statuses := h.router.ListProviders(ctx)
	if len(statuses) == 0 {
		text.WriteString("Providers: none configured\n")
	} else {
		text.WriteString("Providers:\n")
		for _, status := range statuses {
			if status.Available {
				fmt.Fprintf(&text, "  %s — %d models\n", status.Name, status.ModelCount)
			} else {
				fmt.Fprintf(&text, "  %s — unavailable\n", status.Name)
			}
		}
	}
	text.WriteString("Use /model <provider> to list its models, /model <provider>/<model> to set.")

	return commands.Result{Text: text.String(), Outcome: commands.Outcome{Status: "ok", Operation: "model.show"}}
}

// list renders one provider's live model listing, capped at listEntryLimit.
func (h *handler) list(ctx context.Context, provider string) commands.Result {
	provider = normalizeProvider(provider)
	models, err := h.router.ListModels(ctx, provider)
	if err != nil {
		if errors.Is(err, agent.ErrProviderUnknown) {
			return commands.Result{
				Text:    unknownProviderText(provider, h.router.Providers()),
				Outcome: commands.Outcome{Status: "rejected", ReasonCode: "unknown_provider", Operation: "model.list"},
			}
		}
		// The fetch error can embed endpoint and credential data; only the
		// provider name the user typed is safe to repeat.
		return commands.Result{
			Text:    fmt.Sprintf("Models for %s are unavailable right now.", provider),
			Outcome: commands.Outcome{Status: "error", ReasonCode: "provider_unavailable", Operation: "model.list"},
		}
	}
	if len(models) == 0 {
		return commands.Result{Text: fmt.Sprintf("No models reported for %s.", provider), Outcome: commands.Outcome{Status: "ok", Operation: "model.list"}}
	}
	return commands.Result{Text: modelListText(models), Outcome: commands.Outcome{Status: "ok", Operation: "model.list"}}
}

// set validates the provider and model, then persists the conversation pin.
func (h *handler) set(ctx context.Context, req commands.Request, provider, model string) commands.Result {
	if !h.router.HasProvider(provider) {
		return commands.Result{
			Text:    unknownProviderText(provider, h.router.Providers()),
			Outcome: commands.Outcome{Status: "rejected", ReasonCode: "unknown_provider", Operation: "model.set"},
		}
	}
	if !utf8.ValidString(model) || utf8.RuneCountInString(model) > modelRuneLimit {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}
	}
	if err := h.pins.SetSessionModel(ctx, req.Principal.CanonicalUserID, req.SessionKey, provider, model); err != nil {
		// Store errors can carry schema or storage detail; only the fixed
		// failure text is user-facing.
		return commands.Result{
			Text:    "Could not set the model for this conversation.",
			Outcome: commands.Outcome{Status: "error", ReasonCode: "operation_failed", Operation: "model.set"},
		}
	}
	return commands.Result{
		Text:    fmt.Sprintf("Model set to %s/%s for this conversation. It applies from your next message and survives /new until changed.", provider, model),
		Outcome: commands.Outcome{Status: "ok", Operation: "model.set", IsChanged: true, AffectedCount: 1},
	}
}

// modelListText renders entries as plain short lines and names the truncation
// remainder so the operator can reach the full listing from the CLI.
func modelListText(models []llm.ModelEntry) string {
	shown := models
	if len(shown) > listEntryLimit {
		shown = shown[:listEntryLimit]
	}
	var text strings.Builder
	for _, entry := range shown {
		if entry.Window > 0 {
			fmt.Fprintf(&text, "  %s (context %d)\n", entry.ID, entry.Window)
		} else {
			fmt.Fprintf(&text, "  %s\n", entry.ID)
		}
	}
	if len(models) > listEntryLimit {
		fmt.Fprintf(&text, "  …and %d more — oswald model list shows all\n", len(models)-listEntryLimit)
	}
	return strings.TrimRight(text.String(), "\n")
}

// unknownProviderText names the rejected provider and the configured set so
// the user can correct the command without another round trip.
func unknownProviderText(provider string, configured []string) string {
	list := "none"
	if len(configured) > 0 {
		list = strings.Join(configured, ", ")
	}
	return fmt.Sprintf("Unknown provider: %s. Configured: %s.", provider, list)
}

// normalizeProvider trims whitespace and the config-selector custom: prefix;
// the runtime provider identity never carries it.
func normalizeProvider(provider string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(provider), "custom:"))
}
