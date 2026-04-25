package gateway

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/accountlink"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/discord"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/imessage"
	localws "github.com/jonahgcarpenter/oswald-ai/internal/gateway/websocket"
	"github.com/jonahgcarpenter/oswald-ai/internal/metrics"
)

// NewServicesFromConfig creates all enabled gateway services for the current runtime config.
func NewServicesFromConfig(cfg *config.Config, links *accountlink.Service, commands *accountlink.CommandHandler, obs *metrics.Metrics, log *config.Logger) ([]Service, error) {
	services := []Service{
		&localws.Gateway{
			Port:     cfg.Port,
			Links:    links,
			Commands: commands,
			Metrics:  obs,
			Log:      log,
		},
	}

	if cfg.DiscordToken != "" {
		services = append(services, &discord.Gateway{
			Token:    cfg.DiscordToken,
			Links:    links,
			Commands: commands,
			Metrics:  obs,
			Log:      log,
		})
	}

	if cfg.BlueBubblesURL != "" && cfg.BlueBubblesPassword != "" {
		services = append(services, &imessage.Gateway{
			Port:                cfg.IMessagePort,
			WebhookPath:         cfg.IMessageWebhookPath,
			BlueBubblesURL:      cfg.BlueBubblesURL,
			BlueBubblesPassword: cfg.BlueBubblesPassword,
			Links:               links,
			Commands:            commands,
			Metrics:             obs,
			Log:                 log,
		})
	}

	log.Info("Gateways enabled: %s", serviceNames(services))

	return services, nil
}

func serviceNames(services []Service) string {
	names := make([]string, 0, len(services))
	for _, service := range services {
		names = append(names, service.Name())
	}
	return strings.Join(names, ", ")
}
