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
// "imessage" for BlueBubbles chat GUIDs. The provider selector must name a
// provider visible to this config revision, and the custom: prefix is
// stripped before storing. The global block calls it against the global
// providers map; applyProfileChannelOverrides calls it against a profile's
// merged providers map and overlays the result.
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

// applyProfileChannelOverrides validates and overlays a named profile's own
// channel routes on the inherited global map. Routing is keyed by gateway
// ("discord"/"imessage"); a profile entry wins on an id collision and never
// deletes a global entry, so an empty profile map inherits the global map
// unchanged. Validation uses the profile's merged providers map, which is what
// makes a profile-private provider usable while another profile cannot see it.
func applyProfileChannelOverrides(cfg *Config, platforms map[string]platformYAML, providers map[string]providerYAML) error {
	for _, platform := range []string{"discord", "bluebubbles"} {
		settings, ok := platforms[platform]
		if !ok || len(settings.ChannelOverrides) == 0 {
			continue
		}
		gateway := platform
		if platform == "bluebubbles" {
			gateway = "imessage"
		}
		staged := &Config{}
		if err := applyChannelOverrides(staged, platform, gateway, settings.ChannelOverrides, providers); err != nil {
			return err
		}
		cfg.ChannelOverrides = overlayChannelOverrides(cfg.ChannelOverrides, gateway, staged.ChannelOverrides[gateway])
	}
	return nil
}

// overlayChannelOverrides copies the inherited gateway maps and merges one
// gateway's profile-resolved routes over them. The outer map is copied so a
// profile's effective overrides never alias the default profile's map or a
// sibling's; inner maps for untouched gateways stay shared and read-only.
func overlayChannelOverrides(inherited map[string]map[string]ChannelOverride, gateway string, resolved map[string]ChannelOverride) map[string]map[string]ChannelOverride {
	out := make(map[string]map[string]ChannelOverride, len(inherited)+1)
	for name, entries := range inherited {
		out[name] = entries
	}
	merged := make(map[string]ChannelOverride, len(inherited[gateway])+len(resolved))
	for id, override := range inherited[gateway] {
		merged[id] = override
	}
	for id, override := range resolved {
		merged[id] = override
	}
	out[gateway] = merged
	return out
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
