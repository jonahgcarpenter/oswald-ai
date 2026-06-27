package gateway

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/discord"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/imessage"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	localws "github.com/jonahgcarpenter/oswald-ai/internal/gateway/websocket"
)

// NewServicesFromConfig creates all enabled gateway services for the current runtime config.
func NewServicesFromConfig(cfg *config.Config, links *accountlinking.Service, runtimeDeps gatewayruntime.Dependencies, log *config.Logger) ([]Service, error) {
	gatewayLog := log.Server("gateway.bootstrap")
	services := []Service{
		&localws.Gateway{
			Port:    cfg.Port,
			Links:   links,
			Runtime: runtimeDeps,
			Log:     log,
		},
	}

	if cfg.DiscordToken != "" {
		services = append(services, &discord.Gateway{
			Token:   cfg.DiscordToken,
			Links:   links,
			Runtime: runtimeDeps,
			Log:     log,
		})
	}

	if cfg.BlueBubblesURL != "" && cfg.BlueBubblesPassword != "" {
		services = append(services, &imessage.Gateway{
			Port:                cfg.IMessagePort,
			WebhookPath:         cfg.IMessageWebhookPath,
			BlueBubblesURL:      cfg.BlueBubblesURL,
			BlueBubblesPassword: cfg.BlueBubblesPassword,
			Links:               links,
			Runtime:             runtimeDeps,
			Log:                 log,
		})
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
