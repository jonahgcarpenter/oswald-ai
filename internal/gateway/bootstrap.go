package gateway

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/discord"
	localws "github.com/jonahgcarpenter/oswald-ai/internal/gateway/websocket"
)

// NewServicesFromConfig creates all enabled gateway services for the current runtime config.
func NewServicesFromConfig(cfg *config.Config, log *config.Logger) ([]Service, error) {
	services := []Service{
		&localws.Gateway{
			Port: cfg.Port,
			Log:  log,
		},
	}

	if cfg.DiscordToken != "" {
		services = append(services, &discord.Gateway{
			Token: cfg.DiscordToken,
			Log:   log,
		})
	}

	return services, nil
}
