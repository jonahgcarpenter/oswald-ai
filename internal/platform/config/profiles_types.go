package config

import (
	"regexp"
	"time"
)

// ProfileRoute binds a transport-authenticated identity to an operator profile.

type ProfileRoute struct {
	Name     string `yaml:"name"`
	Platform string `yaml:"platform"`
	UserID   string `yaml:"user_id"`
	Profile  string `yaml:"profile"`
}

// AdmissionPolicy controls gateway admission. Banned identities are rejected
// everywhere and take precedence; a nonempty Allowed list admits only those
// identities, while an empty list admits every routed identity.

type AdmissionPolicy struct {
	Allowed []string
	Banned  []string
}

// Bans reports whether an already normalized external identity is banned.

func (p AdmissionPolicy) Bans(identifier string) bool {
	for _, banned := range p.Banned {
		if banned == identifier {
			return true
		}
	}
	return false
}

// Allows reports whether an already normalized external identity may be admitted.

func (p AdmissionPolicy) Allows(identifier string) bool {
	if p.Bans(identifier) {
		return false
	}
	if len(p.Allowed) == 0 {
		return true
	}
	for _, allowed := range p.Allowed {
		if allowed == identifier {
			return true
		}
	}
	return false
}

// DefaultBlueBubblesWebhookPath preserves the historical webhook route when the
// operator configures no webhook_path.
const DefaultBlueBubblesWebhookPath = "/bluebubbles/webhook"

const (
	maxMentionPatterns     = 4
	maxMentionPatternRunes = 512
)

var webhookPathRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,127}$`)
var webhookCallbackLabelRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

type providerYAML struct {
	API  string  `yaml:"api"`
	Key  *string `yaml:"key"`
	Auth string  `yaml:"auth"`
}

type modelYAML struct {
	Provider      *string `yaml:"provider"`
	Default       *string `yaml:"default"`
	ContextLength *int    `yaml:"context_length"`
}

type webYAML struct {
	BraveKey   *string `yaml:"brave_key"`
	SearxngURL *string `yaml:"searxng_url"`
}

type imageYAML struct {
	URL     *string `yaml:"url"`
	Timeout *string `yaml:"generation_timeout"`
}

type browserYAML struct {
	LaunchCommand     *string `yaml:"launch_command"`
	EndpointURL       *string `yaml:"endpoint_url"`
	CommandTimeout    *string `yaml:"command_timeout"`
	SnapshotThreshold *int    `yaml:"snapshot_threshold"`
	InactivityTimeout *string `yaml:"inactivity_timeout"`
	RecordSessions    *bool   `yaml:"record_sessions"`
	AllowPrivateURLs  *bool   `yaml:"allow_private_urls"`
	AllowInsecureURLs *bool   `yaml:"allow_insecure_urls"`
	RestrictEvaluate  *bool   `yaml:"restrict_evaluate"`
}

type terminalYAML struct {
	EnvPassthrough []string `yaml:"env_passthrough"`
}

type toolsYAML struct {
	WebSearch     webYAML      `yaml:"web_search"`
	ImageGenerate imageYAML    `yaml:"image_generate"`
	Browser       browserYAML  `yaml:"browser"`
	Terminal      terminalYAML `yaml:"terminal"`
}

type platformExtraYAML struct {
	Token               *string  `yaml:"token"`
	AllowedUsers        []string `yaml:"allowed_users"`
	BannedUsers         []string `yaml:"banned_users"`
	RequireMention      *bool    `yaml:"require_mention"`
	ServerURL           string   `yaml:"server_url"`
	ServerPassword      *string  `yaml:"server_password"`
	WebhookHost         string   `yaml:"webhook_host"`
	WebhookCallbackHost string   `yaml:"webhook_callback_host"`
	WebhookPort         int      `yaml:"webhook_port"`
	WebhookPath         string   `yaml:"webhook_path"`
	MentionPatterns     []string `yaml:"mention_pattern"`
	APIHost             string   `yaml:"api_host"`
	APIPort             int      `yaml:"api_port"`
}

type modelOverrideYAML struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type platformYAML struct {
	Enabled          bool                         `yaml:"enabled"`
	Extra            platformExtraYAML            `yaml:"extra"`
	ChannelOverrides map[string]modelOverrideYAML `yaml:"channel_overrides"`
}

type apiYAML struct {
	Key *string `yaml:"key"`
}

type documentYAML struct {
	Providers map[string]providerYAML `yaml:"providers"`
	Model     modelYAML               `yaml:"model"`
	API       apiYAML                 `yaml:"api"`
	Gateway   *struct {
		MultiplexProfiles bool           `yaml:"multiplex_profiles"`
		Routes            []ProfileRoute `yaml:"profile_routes"`
	} `yaml:"gateway"`
	Platforms map[string]platformYAML `yaml:"platforms"`
	Tools     toolsYAML               `yaml:"tools"`
	Runtime   *struct {
		LogLevel string `yaml:"log_level"`
	} `yaml:"runtime"`
	MCPServers map[string]mcpServerYAML `yaml:"mcp_servers"`
	Plugins    *pluginsYAML             `yaml:"plugins"`
	Delegation *delegationYAML          `yaml:"delegation"`
}

type mcpToolsYAML struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

type mcpOAuthYAML struct {
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	Flow         string   `yaml:"flow"`
	Scopes       []string `yaml:"scopes"`
	Timeout      string   `yaml:"timeout"`
}

type mcpServerYAML struct {
	URL       string            `yaml:"url"`
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	Cwd       string            `yaml:"cwd"`
	Env       map[string]string `yaml:"env"`
	Transport string            `yaml:"transport"`
	Headers   map[string]string `yaml:"headers"`
	Tools     mcpToolsYAML      `yaml:"tools"`
	Auth      string            `yaml:"auth"`
	OAuth     mcpOAuthYAML      `yaml:"oauth"`
	Enabled   *bool             `yaml:"enabled"`
}

// MCPOAuth holds resolved per-server OAuth parameters. Secrets must not be logged.

type MCPOAuth struct {
	ClientID     string
	ClientSecret string
	Flow         string
	Scopes       []string
	Timeout      time.Duration
}

// MCPServer holds one resolved operator-owned profile connection. It is never
// persisted in SQLite, and its URL/command/headers/environment/secrets must not be logged.

type MCPServer struct {
	Name, URL, Command, Cwd, Transport string
	Args                               []string
	Headers, Env                       map[string]string
	Include, Exclude                   []string
	Auth                               string
	OAuth                              MCPOAuth
	Enabled                            bool
}

var profileNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidProfileName excludes traversal, hidden entries, and the reserved default name.

func ValidProfileName(name string) bool { return name != "default" && profileNameRE.MatchString(name) }
