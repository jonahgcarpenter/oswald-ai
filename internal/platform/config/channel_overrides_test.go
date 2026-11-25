package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const channelOverridePlatformConfig = `providers:
  fake:
    api: "http://localhost:9999/v1"
    key: "${PROFILE_TEST_MODEL_KEY}"
  other:
    api: "http://localhost:9998/v1"
    key: "synthetic-other-key"
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
gateway:
  multiplex_profiles: true
  profile_routes:
    - platform: discord
      user_id: "555000999"
      profile: alice
platforms:
  api:
    enabled: true
    extra:
      api_host: "127.0.0.1"
      api_port: 12345
  discord:
    enabled: true
    extra:
      token: synthetic-discord-token
    channel_overrides:
      "555000111":
        provider: custom:other
        model: other/model
  bluebubbles:
    enabled: true
    extra:
      server_url: http://127.0.0.1:1234
      server_password: synthetic-bluebubbles-password
      webhook_port: 8645
    channel_overrides:
      "iMessage;-;+15550002222":
        provider: custom:fake
        model: fake/chat
`

const channelOverrideBase = `providers:
  fake:
    api: "http://localhost:9999/v1"
    key: "${PROFILE_TEST_MODEL_KEY}"
  other:
    api: "http://localhost:9998/v1"
    key: "synthetic-other-key"
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  discord:
    enabled: true
    extra:
      token: synthetic-discord-token
    channel_overrides:
%s
`

const disabledChannelOverrideBase = `providers:
  fake:
    api: "http://localhost:9999/v1"
    key: "${PROFILE_TEST_MODEL_KEY}"
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  api:
    enabled: true
    extra:
      api_host: 127.0.0.1
      api_port: 12345
  discord:
    enabled: false
    channel_overrides:
%s
`

const apiChannelOverrideConfig = `providers:
  fake:
    api: "http://localhost:9999/v1"
    key: "${PROFILE_TEST_MODEL_KEY}"
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  api:
    enabled: true
    extra:
      api_host: 127.0.0.1
      api_port: 12345
    channel_overrides:
      "555000111":
        provider: custom:fake
        model: fake/model
`

// channelOverrideYAML renders one indented channel_overrides entry.
func channelOverrideYAML(id, provider, model string) string {
	return fmt.Sprintf("      %q:\n        provider: %q\n        model: %q\n", id, provider, model)
}

func TestChannelOverridesResolve(t *testing.T) {
	cfg, err := LoadProfiles(profileConfigFixture(t, channelOverridePlatformConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	wantDiscord := map[string]ChannelOverride{"555000111": {Provider: "other", Model: "other/model"}}
	if !reflect.DeepEqual(cfg.ChannelOverrides["discord"], wantDiscord) {
		t.Fatalf("discord overrides = %+v, want %+v", cfg.ChannelOverrides["discord"], wantDiscord)
	}
	wantIMessage := map[string]ChannelOverride{"iMessage;-;+15550002222": {Provider: "fake", Model: "fake/chat"}}
	if !reflect.DeepEqual(cfg.ChannelOverrides["imessage"], wantIMessage) {
		t.Fatalf("imessage overrides = %+v, want %+v", cfg.ChannelOverrides["imessage"], wantIMessage)
	}
	alice := cfg.Profiles["alice"]
	if alice == nil {
		t.Fatal("named profile missing")
	}
	if !reflect.DeepEqual(alice.ChannelOverrides, cfg.ChannelOverrides) {
		t.Fatalf("named profile overrides = %+v, want the global map", alice.ChannelOverrides)
	}
}

func TestChannelOverridesRejectInvalidEntries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides string
		code      string
		path      string
	}{
		{
			"unknown provider",
			channelOverrideYAML("555000111", "custom:missing", "other/model"),
			"config_model_provider_invalid",
			"platforms.discord.channel_overrides.555000111.provider",
		},
		{
			"missing custom prefix",
			channelOverrideYAML("555000111", "other", "other/model"),
			"config_model_provider_invalid",
			"platforms.discord.channel_overrides.555000111.provider",
		},
		{
			"empty provider",
			channelOverrideYAML("555000111", "", "other/model"),
			"config_model_provider_invalid",
			"platforms.discord.channel_overrides.555000111.provider",
		},
		{
			"empty model",
			channelOverrideYAML("555000111", "custom:other", ""),
			"config_value_invalid",
			"platforms.discord.channel_overrides.555000111.model",
		},
		{
			"oversized model",
			channelOverrideYAML("555000111", "custom:other", strings.Repeat("m", 257)),
			"config_value_invalid",
			"platforms.discord.channel_overrides.555000111.model",
		},
		{
			"empty id",
			channelOverrideYAML("", "custom:other", "other/model"),
			"config_value_invalid",
			"platforms.discord.channel_overrides",
		},
		{
			"oversized id",
			channelOverrideYAML(strings.Repeat("1", 257), "custom:other", "other/model"),
			"config_value_invalid",
			"platforms.discord.channel_overrides",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadProfiles(profileConfigFixture(t, fmt.Sprintf(channelOverrideBase, tc.overrides)))
			configErr, ok := AsConfigError(err)
			if !ok || configErr.Code != tc.code || configErr.Path != tc.path {
				t.Fatalf("validation mismatch: %+v err=%v", configErr, err)
			}
		})
	}
}

func TestChannelOverridesBoundEntryCount(t *testing.T) {
	var entries strings.Builder
	for i := 0; i < maxChannelOverrides+1; i++ {
		entries.WriteString(channelOverrideYAML(fmt.Sprintf("555%06d", i), "custom:other", "other/model"))
	}
	_, err := LoadProfiles(profileConfigFixture(t, fmt.Sprintf(channelOverrideBase, entries.String())))
	configErr, ok := AsConfigError(err)
	if !ok || configErr.Code != "config_value_invalid" || configErr.Path != "platforms.discord.channel_overrides" {
		t.Fatalf("entry-count validation mismatch: %+v err=%v", configErr, err)
	}
}

func TestChannelOverridesRejectedOnAPIPlatform(t *testing.T) {
	_, err := LoadProfiles(profileConfigFixture(t, apiChannelOverrideConfig))
	configErr, ok := AsConfigError(err)
	if !ok || configErr.Code != "config_value_invalid" || configErr.Path != "platforms.api.channel_overrides" {
		t.Fatalf("api override validation mismatch: %+v err=%v", configErr, err)
	}
}

func TestChannelOverridesDisabledPlatformKeyShape(t *testing.T) {
	// A structurally valid disabled block stays pruned: its provider is not
	// resolved and its overrides never reach the effective config.
	cfg, err := LoadProfiles(profileConfigFixture(t, fmt.Sprintf(disabledChannelOverrideBase, channelOverrideYAML("555000111", "custom:undeclared", "ghost/model"))))
	if err != nil {
		t.Fatalf("disabled platform override resolution failed: %v", err)
	}
	if len(cfg.ChannelOverrides) != 0 {
		t.Fatalf("disabled platform overrides survived pruning: %+v", cfg.ChannelOverrides)
	}

	// Unknown keys inside a disabled block are still rejected.
	_, err = LoadProfiles(profileConfigFixture(t, fmt.Sprintf(disabledChannelOverrideBase, "      \"555000111\":\n        provider: custom:undeclared\n        model: ghost/model\n        temperature: 1\n")))
	configErr, ok := AsConfigError(err)
	if !ok || configErr.Code != "config_key_unknown" || configErr.Path != "platforms.discord.channel_overrides.555000111.temperature" {
		t.Fatalf("disabled block key-shape mismatch: %+v err=%v", configErr, err)
	}

	// A non-mapping channel_overrides block is rejected before value
	// resolution as well.
	_, err = LoadProfiles(profileConfigFixture(t, strings.Replace(disabledChannelOverrideBase, "    channel_overrides:\n%s", "    channel_overrides: nope", 1)))
	configErr, ok = AsConfigError(err)
	if !ok || configErr.Code != "config_key_invalid" || configErr.Path != "platforms.discord.channel_overrides" {
		t.Fatalf("disabled block shape mismatch: %+v err=%v", configErr, err)
	}
}

func TestChannelOverridesNamedProfilePlacementRejected(t *testing.T) {
	root := profileConfigFixture(t, channelOverridePlatformConfig)
	path := filepath.Join(root, "profiles", "alice", "config.yaml")
	if err := os.WriteFile(path, []byte("platforms:\n  discord:\n    channel_overrides:\n      \"555000111\":\n        provider: custom:other\n        model: other/model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadProfiles(root)
	configErr, ok := AsConfigError(err)
	if !ok || configErr.Code != "config_profile_override_invalid" || configErr.Path != "platforms" {
		t.Fatalf("named profile override accepted: %+v err=%v", configErr, err)
	}
}
