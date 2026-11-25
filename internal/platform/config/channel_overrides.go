package config

import (
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Channel override bounds. The map stays small because every entry is an
// operator-managed route; each id and value matches the persisted pin bound.
const (
	maxChannelOverrides       = 64
	maxChannelOverrideIDRunes = 256
	maxChannelOverrideRunes   = 256
)

// ChannelOverride routes one platform channel to a provider/model pair.
// Provider is the bare provider name (the custom: selector prefix is
// validated and stripped at load).
type ChannelOverride struct {
	Provider string
	Model    string
}

// applyChannelOverrides validates one platform's channel_overrides map into
// cfg, keyed by internal gateway name: "discord" for Discord channel ids and
// "imessage" for BlueBubbles chat GUIDs. The provider selector must name an
// already declared global provider, and the custom: prefix is stripped before
// storing. Overrides are global-only, so named profiles inherit the resolved
// map and can never set their own.
func applyChannelOverrides(cfg *Config, platform, gateway string, raw map[string]modelOverrideYAML, providers map[string]providerYAML) error {
	if len(raw) == 0 {
		return nil
	}
	base := "platforms." + platform + ".channel_overrides"
	if len(raw) > maxChannelOverrides {
		return configErr("config_value_invalid", base)
	}
	resolved := make(map[string]ChannelOverride, len(raw))
	for id, override := range raw {
		if id == "" || !utf8.ValidString(id) || utf8.RuneCountInString(id) > maxChannelOverrideIDRunes {
			return configErr("config_value_invalid", base)
		}
		providerPath := base + "." + id + ".provider"
		if !strings.HasPrefix(override.Provider, "custom:") {
			return configErr("config_model_provider_invalid", providerPath)
		}
		providerName := strings.TrimPrefix(override.Provider, "custom:")
		if _, ok := providers[providerName]; !ok {
			return configErr("config_model_provider_invalid", providerPath)
		}
		modelPath := base + "." + id + ".model"
		if override.Model == "" || !utf8.ValidString(override.Model) || utf8.RuneCountInString(override.Model) > maxChannelOverrideRunes {
			return configErr("config_value_invalid", modelPath)
		}
		resolved[id] = ChannelOverride{Provider: providerName, Model: override.Model}
	}
	if cfg.ChannelOverrides == nil {
		cfg.ChannelOverrides = make(map[string]map[string]ChannelOverride)
	}
	cfg.ChannelOverrides[gateway] = resolved
	return nil
}

// validateChannelOverrideKeys rejects unknown nested keys without resolving
// values, so a disabled platform block still reports a typo.
func validateChannelOverrideKeys(base string, node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return configErr("config_key_invalid", base)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		entry := node.Content[i+1]
		entryPath := base + "." + node.Content[i].Value
		if entry.Kind != yaml.MappingNode {
			return configErr("config_key_invalid", entryPath)
		}
		for j := 0; j+1 < len(entry.Content); j += 2 {
			switch entry.Content[j].Value {
			case "provider", "model":
			default:
				return configErr("config_key_unknown", entryPath+"."+entry.Content[j].Value)
			}
		}
	}
	return nil
}
