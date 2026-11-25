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

// profileChannelOverrideBase enables both transport platforms and an API
// listener but declares no global routes, so profile overlays start empty.
const profileChannelOverrideBase = `providers:
  fake:
    api: "http://localhost:9999/v1"
    key: "${PROFILE_TEST_MODEL_KEY}"
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
  bluebubbles:
    enabled: true
    extra:
      server_url: http://127.0.0.1:1234
      server_password: synthetic-bluebubbles-password
      webhook_port: 8645
`

// profileChannelOverrideGlobal adds a global baseline route per platform so
// profile overlays can be checked against inheritance and collisions.
const profileChannelOverrideGlobal = `providers:
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
        provider: custom:fake
        model: fake/global
  bluebubbles:
    enabled: true
    extra:
      server_url: http://127.0.0.1:1234
      server_password: synthetic-bluebubbles-password
      webhook_port: 8645
    channel_overrides:
      "iMessage;-;+15550001111":
        provider: custom:fake
        model: fake/global
`

// profileChannelOverrideTwoProfiles routes two named profiles so isolation
// between their provider maps can be exercised.
const profileChannelOverrideTwoProfiles = `providers:
  fake:
    api: "http://localhost:9999/v1"
    key: "${PROFILE_TEST_MODEL_KEY}"
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
    - platform: discord
      user_id: "555000888"
      profile: bob
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
`

// writeProfileConfig writes one named profile override document.
func writeProfileConfig(t *testing.T, root, name, text string) {
	t.Helper()
	dir := filepath.Join(root, "profiles", name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProfileChannelOverridesResolveWithPrivateProvider(t *testing.T) {
	root := profileConfigFixture(t, profileChannelOverrideBase)
	writeProfileConfig(t, root, "alice", `providers:
  bifrost_opencode:
    api: "http://localhost:9997/v1"
    key: "synthetic-bifrost-key"
platforms:
  discord:
    channel_overrides:
      "555000222":
        provider: custom:bifrost_opencode
        model: bifrost/model
  bluebubbles:
    channel_overrides:
      "iMessage;-;+15550002222":
        provider: custom:bifrost_opencode
        model: bifrost/chat
`)
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.ChannelOverrides) != 0 {
		t.Fatalf("default profile gained profile routes: %+v", cfg.ChannelOverrides)
	}
	alice := cfg.Profiles["alice"]
	if alice == nil {
		t.Fatal("named profile missing")
	}
	wantDiscord := map[string]ChannelOverride{"555000222": {Provider: "bifrost_opencode", Model: "bifrost/model"}}
	if !reflect.DeepEqual(alice.ChannelOverrides["discord"], wantDiscord) {
		t.Fatalf("profile discord overrides = %+v, want %+v", alice.ChannelOverrides["discord"], wantDiscord)
	}
	wantIMessage := map[string]ChannelOverride{"iMessage;-;+15550002222": {Provider: "bifrost_opencode", Model: "bifrost/chat"}}
	if !reflect.DeepEqual(alice.ChannelOverrides["imessage"], wantIMessage) {
		t.Fatalf("profile imessage overrides = %+v, want %+v", alice.ChannelOverrides["imessage"], wantIMessage)
	}
}

func TestProfileChannelOverridesValidateWhenGlobalPlatformDisabled(t *testing.T) {
	global := strings.Replace(profileChannelOverrideBase, "  discord:\n    enabled: true", "  discord:\n    enabled: false", 1)
	for _, tc := range []struct {
		name    string
		profile string
		code    string
		path    string
	}{
		{
			"unknown nested key",
			"platforms:\n  discord:\n    channel_overrides:\n      \"555000222\":\n        provider: custom:fake\n        model: fake/model\n        temperature: 1\n",
			"config_key_unknown",
			"platforms.discord.channel_overrides.555000222.temperature",
		},
		{
			"unknown provider",
			"platforms:\n  discord:\n    channel_overrides:\n      \"555000222\":\n        provider: custom:missing\n        model: ghost/model\n",
			"config_model_provider_invalid",
			"platforms.discord.channel_overrides.555000222.provider",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := profileConfigFixture(t, global)
			writeProfileConfig(t, root, "alice", tc.profile)
			_, err := LoadProfiles(root)
			configErr, ok := AsConfigError(err)
			if !ok || configErr.Code != tc.code || configErr.Path != tc.path {
				t.Fatalf("disabled-global validation mismatch: %+v err=%v", configErr, err)
			}
		})
	}
}

func TestProfileChannelOverridesMergeGlobalBaseline(t *testing.T) {
	root := profileConfigFixture(t, profileChannelOverrideGlobal)
	writeProfileConfig(t, root, "alice", `platforms:
  discord:
    channel_overrides:
      "555000111":
        provider: custom:other
        model: other/won
      "555000222":
        provider: custom:other
        model: other/alice
`)
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	wantDefaultDiscord := map[string]ChannelOverride{"555000111": {Provider: "fake", Model: "fake/global"}}
	if !reflect.DeepEqual(cfg.ChannelOverrides["discord"], wantDefaultDiscord) {
		t.Fatalf("default discord overrides = %+v, want %+v", cfg.ChannelOverrides["discord"], wantDefaultDiscord)
	}
	alice := cfg.Profiles["alice"]
	if alice == nil {
		t.Fatal("alice profile missing")
	}
	wantDiscord := map[string]ChannelOverride{
		"555000111": {Provider: "other", Model: "other/won"},
		"555000222": {Provider: "other", Model: "other/alice"},
	}
	if !reflect.DeepEqual(alice.ChannelOverrides["discord"], wantDiscord) {
		t.Fatalf("profile discord overrides = %+v, want %+v", alice.ChannelOverrides["discord"], wantDiscord)
	}
	// The profile adds nothing for bluebubbles, so it inherits the global map.
	wantIMessage := map[string]ChannelOverride{"iMessage;-;+15550001111": {Provider: "fake", Model: "fake/global"}}
	if !reflect.DeepEqual(alice.ChannelOverrides["imessage"], wantIMessage) {
		t.Fatalf("profile imessage overrides = %+v, want the inherited map", alice.ChannelOverrides["imessage"])
	}
	if !reflect.DeepEqual(cfg.ChannelOverrides["imessage"], wantIMessage) {
		t.Fatalf("default imessage overrides = %+v, want %+v", cfg.ChannelOverrides["imessage"], wantIMessage)
	}
}

func TestProfileChannelOverridesRejectInvalidShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		code    string
		path    string
	}{
		{
			"unknown provider",
			"platforms:\n  discord:\n    channel_overrides:\n      \"555000222\":\n        provider: custom:missing\n        model: ghost/model\n",
			"config_model_provider_invalid",
			"platforms.discord.channel_overrides.555000222.provider",
		},
		{
			"missing custom prefix",
			"platforms:\n  discord:\n    channel_overrides:\n      \"555000222\":\n        provider: fake\n        model: fake/model\n",
			"config_model_provider_invalid",
			"platforms.discord.channel_overrides.555000222.provider",
		},
		{
			"enabled flag",
			"platforms:\n  discord:\n    enabled: true\n",
			"config_profile_override_invalid",
			"platforms.discord.enabled",
		},
		{
			"extra block",
			"platforms:\n  discord:\n    extra:\n      token: synthetic-other-token\n",
			"config_profile_override_invalid",
			"platforms.discord.extra",
		},
		{
			"api channel overrides",
			"platforms:\n  api:\n    channel_overrides:\n      \"555000222\":\n        provider: custom:fake\n        model: fake/model\n",
			"config_profile_override_invalid",
			"platforms.api",
		},
		{
			"api enabled",
			"platforms:\n  api:\n    enabled: true\n",
			"config_profile_override_invalid",
			"platforms.api",
		},
		{
			"unknown platform",
			"platforms:\n  telegram:\n    channel_overrides: {}\n",
			"config_profile_override_invalid",
			"platforms.telegram",
		},
		{
			"empty platform entry",
			"platforms:\n  discord: {}\n",
			"config_profile_override_invalid",
			"platforms.discord",
		},
		{
			"unknown override key",
			"platforms:\n  discord:\n    channel_overrides:\n      \"555000222\":\n        provider: custom:fake\n        model: fake/model\n        temperature: 1\n",
			"config_key_unknown",
			"platforms.discord.channel_overrides.555000222.temperature",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := profileConfigFixture(t, profileChannelOverrideBase)
			writeProfileConfig(t, root, "alice", tc.profile)
			_, err := LoadProfiles(root)
			configErr, ok := AsConfigError(err)
			if !ok || configErr.Code != tc.code || configErr.Path != tc.path {
				t.Fatalf("validation mismatch: %+v err=%v", configErr, err)
			}
		})
	}
}

func TestProfileChannelOverridesIsolateProfiles(t *testing.T) {
	root := profileConfigFixture(t, profileChannelOverrideTwoProfiles)
	writeProfileConfig(t, root, "alice", "platforms:\n  discord:\n    channel_overrides:\n      \"555000222\":\n        provider: custom:fake\n        model: fake/alice\n")
	writeProfileConfig(t, root, "bob", "platforms:\n  discord:\n    channel_overrides:\n      \"555000333\":\n        provider: custom:fake\n        model: fake/bob\n")
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.ChannelOverrides) != 0 {
		t.Fatalf("default profile gained profile routes: %+v", cfg.ChannelOverrides)
	}
	wantAlice := map[string]ChannelOverride{"555000222": {Provider: "fake", Model: "fake/alice"}}
	alice := cfg.Profiles["alice"]
	if alice == nil {
		t.Fatal("alice profile missing")
	}
	if !reflect.DeepEqual(alice.ChannelOverrides["discord"], wantAlice) {
		t.Fatalf("alice overrides = %+v, want %+v", alice.ChannelOverrides["discord"], wantAlice)
	}
	wantBob := map[string]ChannelOverride{"555000333": {Provider: "fake", Model: "fake/bob"}}
	bob := cfg.Profiles["bob"]
	if bob == nil {
		t.Fatal("bob profile missing")
	}
	if !reflect.DeepEqual(bob.ChannelOverrides["discord"], wantBob) {
		t.Fatalf("bob overrides = %+v, want %+v", bob.ChannelOverrides["discord"], wantBob)
	}
}

func TestProfileChannelOverrideRejectsSiblingPrivateProvider(t *testing.T) {
	root := profileConfigFixture(t, profileChannelOverrideTwoProfiles)
	writeProfileConfig(t, root, "alice", `providers:
  onlyalice:
    api: "http://localhost:9997/v1"
    key: "synthetic-alice-key"
platforms:
  discord:
    channel_overrides:
      "555000222":
        provider: custom:onlyalice
        model: alice/model
`)
	writeProfileConfig(t, root, "bob", "platforms:\n  discord:\n    channel_overrides:\n      \"555000333\":\n        provider: custom:onlyalice\n        model: alice/model\n")
	_, err := LoadProfiles(root)
	configErr, ok := AsConfigError(err)
	if !ok || configErr.Code != "config_model_provider_invalid" || configErr.Path != "platforms.discord.channel_overrides.555000333.provider" {
		t.Fatalf("sibling provider leaked across profiles: %+v err=%v", configErr, err)
	}
}
