package config

import (
	"strings"
	"testing"
)

const policyPlatformConfig = `providers:
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
      user_id: "123"
      profile: alice
platforms:
  discord:
    enabled: true
    extra:
      token: ${PROFILE_TEST_DISCORD_TOKEN}
      allowed_users:
        - "123"
        - "123"
      banned_users:
        - "456"
      require_mention: ${PROFILE_TEST_REQUIRE_MENTION}
  bluebubbles:
    enabled: true
    extra:
      server_url: http://192.168.4.206:1234
      server_password: ${PROFILE_TEST_BLUEBUBBLES_PASSWORD}
      webhook_host: 0.0.0.0
      webhook_port: 8090
      webhook_path: /bluebubbles-webhook
      allowed_users:
        - "+16623344454"
      banned_users:
        - "16628876635"
      require_mention: false
      mention_pattern:
        - '@?Oswald\b[,:\-]?'
`

func blueBubblesPolicyFixture(t *testing.T, extra string) string {
	t.Helper()
	return profileConfigFixture(t, `providers:
  fake:
    api: "http://localhost:9999/v1"
    key: "${PROFILE_TEST_MODEL_KEY}"
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
gateway:
  multiplex_profiles: true
platforms:
  bluebubbles:
    enabled: true
    extra:
      server_url: http://127.0.0.1:1234
      server_password: synthetic-password
      webhook_port: 8645
`+extra)
}

func TestPlatformAdmissionAndMentionConfiguration(t *testing.T) {
	root := profileConfigFixture(t, policyPlatformConfig)
	t.Setenv("PROFILE_TEST_DISCORD_TOKEN", "synthetic-discord")
	t.Setenv("PROFILE_TEST_BLUEBUBBLES_PASSWORD", "synthetic-bluebubbles")
	t.Setenv("PROFILE_TEST_REQUIRE_MENTION", "false")

	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.DiscordPolicy.Allowed) != 1 || cfg.DiscordPolicy.Allowed[0] != "123" {
		t.Fatalf("discord allow list not normalized/deduplicated: %+v", cfg.DiscordPolicy)
	}
	if !cfg.DiscordPolicy.Allows("123") || cfg.DiscordPolicy.Allows("456") {
		t.Fatalf("discord admission semantics wrong: %+v", cfg.DiscordPolicy)
	}
	if cfg.DiscordGroupRequireMention {
		t.Fatal("require_mention environment value was not applied")
	}
	if cfg.BlueBubblesGroupRequireMention {
		t.Fatal("bluebubbles require_mention: false ignored")
	}
	if cfg.BlueBubblesWebhookPath != "/bluebubbles-webhook" {
		t.Fatalf("webhook_path = %q", cfg.BlueBubblesWebhookPath)
	}
	if len(cfg.BlueBubblesPolicy.Banned) != 1 || cfg.BlueBubblesPolicy.Banned[0] != "+16628876635" {
		t.Fatalf("banned identity not normalized: %+v", cfg.BlueBubblesPolicy)
	}
	if !cfg.BlueBubblesPolicy.Allows("+16623344454") || cfg.BlueBubblesPolicy.Allows("+16628876635") {
		t.Fatalf("bluebubbles admission semantics wrong: %+v", cfg.BlueBubblesPolicy)
	}
	if len(cfg.BlueBubblesMentionPatterns) != 1 || !cfg.BlueBubblesMentionPatterns[0].MatchString("@Oswald, hi") {
		t.Fatalf("mention pattern not compiled: %+v", cfg.BlueBubblesMentionPatterns)
	}
	if cfg.BlueBubblesMentionPatterns[0].MatchString("hi @Oswald") {
		t.Fatal("configured mention pattern was not anchored to the start")
	}
}

func TestAdmissionPolicySemantics(t *testing.T) {
	if !(AdmissionPolicy{}).Allows("anyone") {
		t.Fatal("empty allow list must admit every routed identity")
	}
	policy := AdmissionPolicy{Allowed: []string{"a"}}
	if policy.Allows("b") || !policy.Allows("a") {
		t.Fatal("nonempty allow list must admit only listed identities")
	}
	banned := AdmissionPolicy{Allowed: []string{"a"}, Banned: []string{"a"}}
	if banned.Allows("a") || !banned.Bans("a") {
		t.Fatal("banned identity must win over the allow list")
	}
}

func TestLegacyPlatformKeysAreRejected(t *testing.T) {
	for _, key := range []string{"dm_policy", "allow_from", "guild_require_mention", "group_require_mention", "dm_mention"} {
		t.Run(key, func(t *testing.T) {
			root := blueBubblesPolicyFixture(t, "      "+key+": value\n")
			_, err := LoadProfiles(root)
			configErr, ok := AsConfigError(err)
			if !ok || configErr.Code != "config_key_unknown" || !strings.Contains(configErr.Path, key) {
				t.Fatalf("legacy key %q was not rejected: %v", key, err)
			}
		})
	}
}

func TestMentionPatternAndWebhookPathValidation(t *testing.T) {
	for _, tc := range []struct{ name, extra, wantPath string }{
		{"lookbehind unsupported", "      mention_pattern:\n        - '(?<![\\w@])@?Oswald\\b'\n", "platforms.bluebubbles.extra.mention_pattern"},
		{"empty match", "      mention_pattern:\n        - 'a*'\n", "platforms.bluebubbles.extra.mention_pattern"},
		{"too many patterns", "      mention_pattern:\n        - a\n        - b\n        - c\n        - d\n        - e\n", "platforms.bluebubbles.extra.mention_pattern"},
		{"relative webhook path", "      webhook_path: bluebubbles\n", "platforms.bluebubbles.extra.webhook_path"},
		{"traversal webhook path", "      webhook_path: /../../etc\n", "platforms.bluebubbles.extra.webhook_path"},
		{"bad banned identity", "      banned_users:\n        - not-a-phone\n", "platforms.bluebubbles.extra.banned_users"},
		{"bad allowed identity", "      allowed_users:\n        - not-a-phone\n", "platforms.bluebubbles.extra.allowed_users"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := blueBubblesPolicyFixture(t, tc.extra)
			_, err := LoadProfiles(root)
			configErr, ok := AsConfigError(err)
			if !ok || configErr.Code != "config_value_invalid" || configErr.Path != tc.wantPath {
				t.Fatalf("validation mismatch: %+v err=%v", configErr, err)
			}
		})
	}
}

func TestWebhookPathDefaultsWhenOmitted(t *testing.T) {
	root := blueBubblesPolicyFixture(t, "")
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.BlueBubblesWebhookPath != DefaultBlueBubblesWebhookPath {
		t.Fatalf("default webhook path = %q", cfg.BlueBubblesWebhookPath)
	}
	if len(cfg.BlueBubblesMentionPatterns) != 0 {
		t.Fatal("omitted mention_pattern should keep built-in defaults in the gateway")
	}
}
