package startup

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/compaction"
)

// modelRoutingHandles are the live per-profile runtime views a model-routing
// reload rotates: the agent's router, the pinned summarizer, and the delegation
// manager's live config. Listeners, tool backends, and MCP sessions stay on
// their startup revision until a restart.
type modelRoutingHandles struct {
	router     *agent.ModelRouter
	summarizer summarizerRotator
	delegate   delegateConfigSwapper
}

// summarizerRotator re-seats a profile's markdown summarizer on a new model or
// transport. *summarizerHolder satisfies it; tests substitute fakes.
type summarizerRotator interface {
	Rotate(summarizerTarget)
}

// delegateConfigSwapper swaps a profile's live delegation configuration.
// *managerdelegate.Manager satisfies it; tests substitute fakes.
type delegateConfigSwapper interface {
	SwapConfig(*config.Config)
}

// buildModelApplier returns the Reloader hook that rotates live model routing
// for every profile present before and after a hot reload. Profiles are keyed
// by name; old and next configs come from the root config for the default
// profile and from the Profiles map for named profiles. A profile absent from
// either side is skipped (classification already makes add/remove non-hot).
// The hook never writes operator files: it only mutates in-memory runtime
// views. The new summarizer is built before any view is touched, so a failure
// WARNs `config.reload.apply_failed`, leaves the profile's router, summarizer,
// and delegation config on the previous revision, and continues with the rest.
func buildModelApplier(handles map[string]modelRoutingHandles, rootLog *config.Logger) func(old, next *config.Config) {
	log := rootLog.Server("app")
	return func(old, next *config.Config) {
		for name, handle := range handles {
			oldCfg := modelRoutingConfig(old, name)
			nextCfg := modelRoutingConfig(next, name)
			if oldCfg == nil || nextCfg == nil {
				continue
			}
			if !modelRoutingChanged(oldCfg, nextCfg) {
				continue
			}
			client := llm.NewGatewayClient(nextCfg.LLMGatewayURL, nextCfg.LLMGatewayAPIKey, nextCfg.LLMGatewayVirtualKey, rootLog)
			summarizer, err := compaction.NewSummarizer(client, nextCfg.LLMGatewayModel)
			if err != nil {
				log.Warn("config.reload.apply_failed", "failed to apply live model routing; keeping previous runtime view", config.F("profile", name), config.ErrorField(err))
				continue
			}
			handle.router.SwapView(modelViewFor(nextCfg))
			handle.summarizer.Rotate(summarizer)
			handle.delegate.SwapConfig(nextCfg)
		}
	}
}

// modelRoutingConfig returns one profile's effective config, or nil when the
// name is absent from that revision.
func modelRoutingConfig(root *config.Config, name string) *config.Config {
	if root == nil {
		return nil
	}
	if name == "default" {
		return root
	}
	return root.Profiles[name]
}

// modelRoutingChanged reports whether the four hot routing fields differ.
func modelRoutingChanged(old, next *config.Config) bool {
	return old.LLMGatewayModel != next.LLMGatewayModel ||
		old.LLMGatewayProvider != next.LLMGatewayProvider ||
		old.LLMGatewayURL != next.LLMGatewayURL ||
		old.LLMGatewayAPIKey != next.LLMGatewayAPIKey
}
