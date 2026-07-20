package gateway

import (
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/discord"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/imessage"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	localws "github.com/jonahgcarpenter/oswald-ai/internal/gateway/websocket"
	"github.com/jonahgcarpenter/oswald-ai/internal/websocketauth"
)

// NewServicesFromConfig creates all enabled gateway services for the current runtime config.
func NewServicesFromConfig(cfg *config.Config, links *accountlinking.Service, auth *websocketauth.Store, runtimeDeps gatewayruntime.Dependencies, log *config.Logger) ([]Service, error) {
	gatewayLog := log.Server("gateway.bootstrap")
	if auth == nil {
		return nil, fmt.Errorf("configure websocket authentication: durable authorization store is required")
	}
	services := []Service{
		&localws.Gateway{
			Port:    cfg.Port,
			Auth:    auth,
			Links:   links,
			Runtime: runtimeDeps,
			Log:     log,
		},
	}
	if runtimeDeps.PrivacyBus != nil {
		runtimeDeps.PrivacyBus.Subscribe(services[0].(*localws.Gateway).HandlePrivacyInvalidation)
	}

	if cfg.DiscordToken != "" {
		discordGateway := &discord.Gateway{
			Token:   cfg.DiscordToken,
			Links:   links,
			Runtime: runtimeDeps,
			Log:     log,
		}
		services = append(services, discordGateway)
		if runtimeDeps.PrivacyBus != nil {
			runtimeDeps.PrivacyBus.Subscribe(discordGateway.HandlePrivacyInvalidation)
		}
	}

	if cfg.BlueBubblesURL != "" && cfg.BlueBubblesPassword != "" {
		iMessageGateway := &imessage.Gateway{
			Port:                cfg.IMessagePort,
			WebhookPath:         cfg.IMessageWebhookPath,
			BlueBubblesURL:      cfg.BlueBubblesURL,
			BlueBubblesPassword: cfg.BlueBubblesPassword,
			Links:               links,
			Runtime:             runtimeDeps,
			Log:                 log,
		}
		services = append(services, iMessageGateway)
		if runtimeDeps.PrivacyBus != nil {
			runtimeDeps.PrivacyBus.Subscribe(iMessageGateway.HandlePrivacyInvalidation)
		}
	}

	gatewayLog.Info("gateway.bootstrap.enabled", "resolved enabled gateways",
		config.F("gateway_count", len(services)),
		config.F("gateways", serviceNames(services)),
	)

	return services, nil
}

func serviceNames(services []Service) string {
	names := make([]string, 0, len(services))
	for _, service := range services {
		names = append(names, service.Name())
	}
	return strings.Join(names, ", ")
}
