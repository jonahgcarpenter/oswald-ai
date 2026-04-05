package gateway

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/accountlink"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/discord"
	localws "github.com/jonahgcarpenter/oswald-ai/internal/gateway/websocket"
)

// NewServicesFromConfig creates all enabled gateway services for the current runtime config.
func NewServicesFromConfig(cfg *config.Config, links *accountlink.Service, commands *accountlink.CommandHandler, log *config.Logger) ([]Service, error) {
	services := []Service{
		&localws.Gateway{
			Port:     cfg.Port,
			Links:    links,
			Commands: commands,
			Log:      log,
		},
	}

	if cfg.DiscordToken != "" {
		services = append(services, &discord.Gateway{
			Token:    cfg.DiscordToken,
			Links:    links,
			Commands: commands,
			Log:      log,
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
