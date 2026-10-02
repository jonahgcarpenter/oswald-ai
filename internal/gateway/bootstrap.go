package gateway

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/discord"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/homeassistant"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/imessage"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/openai"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

// NewServicesFromConfig creates all enabled gateway services for the current runtime config.
func NewServicesFromConfig(cfg *config.Config, links identity.Resolver, runtimeDeps gatewayruntime.Dependencies, log *config.Logger) ([]Service, error) {
	gatewayLog := log.Server("gateway.bootstrap")
	services := make([]Service, 0, 4)
	if strings.TrimSpace(cfg.OpenAIListenPort) != "" {
		api, err := openai.New(cfg.OpenAIListenPort, links, runtimeDeps, cfg.LLMGatewayModel, log)
		if err != nil {
			gatewayLog.Warn("gateway.openai.config_invalid", "openai gateway configuration is invalid; gateway disabled", config.F("status", "degraded"), config.ErrorField(err))
		} else {
			services = append(services, api)
		}
	}
	homeAssistantTokenSet := strings.TrimSpace(cfg.HomeAssistantAuthToken) != ""
	homeAssistantPortSet := strings.TrimSpace(cfg.HomeAssistantListenPort) != ""
	if homeAssistantTokenSet && homeAssistantPortSet {
		homeAssistantGateway, err := homeassistant.New(cfg.HomeAssistantListenPort, cfg.HomeAssistantAuthToken, links, runtimeDeps, log)
		if err != nil {
			gatewayLog.Warn("gateway.homeassistant.config_invalid", "home assistant gateway configuration is invalid; gateway disabled", config.F("status", "degraded"), config.ErrorField(err))
		} else {
			services = append(services, homeAssistantGateway)
		}
	} else {
		gatewayLog.Debug("gateway.homeassistant.disabled", "home assistant gateway is disabled", config.F("is_token_set", homeAssistantTokenSet), config.F("is_port_set", homeAssistantPortSet))
	}

	discordToken := strings.TrimSpace(cfg.DiscordToken)
	if discordToken != "" {
		discordGateway := &discord.Gateway{
			Token:   discordToken,
			Links:   links,
			Runtime: runtimeDeps,
			Log:     log,
		}
		services = append(services, discordGateway)
	} else {
		gatewayLog.Debug("gateway.discord.disabled", "discord gateway is disabled", config.F("is_token_set", false))
	}

	blueBubblesPortSet := strings.TrimSpace(cfg.BlueBubblesListenPort) != ""
	blueBubblesURLSet := strings.TrimSpace(cfg.BlueBubblesURL) != ""
	blueBubblesPasswordSet := strings.TrimSpace(cfg.BlueBubblesPassword) != ""
	if blueBubblesPortSet && blueBubblesURLSet && blueBubblesPasswordSet {
		port, baseURL, err := validateBlueBubblesConfig(cfg.BlueBubblesListenPort, cfg.BlueBubblesURL)
		if err != nil {
			gatewayLog.Warn("gateway.imessage.config_invalid", "imessage gateway configuration is invalid; gateway disabled", config.F("status", "degraded"), config.ErrorField(err))
		} else {
			iMessageGateway := &imessage.Gateway{
				Port:                port,
				BlueBubblesURL:      baseURL,
				BlueBubblesPassword: cfg.BlueBubblesPassword,
				DMMention:           cfg.BlueBubblesDMMention,
				Links:               links,
				Runtime:             runtimeDeps,
				Log:                 log,
			}
			services = append(services, iMessageGateway)
		}
	} else {
		gatewayLog.Debug("gateway.imessage.disabled", "imessage gateway is disabled", config.F("is_port_set", blueBubblesPortSet), config.F("is_url_set", blueBubblesURLSet), config.F("is_password_set", blueBubblesPasswordSet))
	}

	if len(services) == 0 {
		return nil, fmt.Errorf("no gateways are configured correctly")
	}

	gatewayLog.Info("gateway.bootstrap.enabled", "resolved enabled gateways",
		config.F("gateway_count", len(services)),
		config.F("gateways", serviceNames(services)),
	)

	return services, nil
}

// OpenAIEnabled reports whether the configured port can enable the loopback listener.
func OpenAIEnabled(cfg *config.Config) bool {
	port, err := strconv.Atoi(strings.TrimSpace(cfg.OpenAIListenPort))
	return err == nil && port >= 1 && port <= 65535
}

func validateBlueBubblesConfig(portValue, urlValue string) (string, string, error) {
	portNumber, err := strconv.Atoi(strings.TrimSpace(portValue))
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", fmt.Errorf("BLUEBUBBLES_LISTEN_PORT must be an integer from 1 through 65535")
	}
	baseURL := strings.TrimSpace(urlValue)
	parsed, err := url.Parse(baseURL)
	if err != nil || !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("BLUEBUBBLES_URL must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	return strconv.Itoa(portNumber), strings.TrimRight(baseURL, "/"), nil
}

func serviceNames(services []Service) string {
	names := make([]string, 0, len(services))
	for _, service := range services {
		names = append(names, service.Name())
	}
	return strings.Join(names, ", ")
}
