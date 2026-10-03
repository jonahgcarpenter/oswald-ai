package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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

type providerYAML struct {
	API string  `yaml:"api"`
	Key *string `yaml:"key"`
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
type toolsYAML struct {
	WebSearch     webYAML   `yaml:"web_search"`
	ImageGenerate imageYAML `yaml:"image_generate"`
}
type platformExtraYAML struct {
	Token           *string  `yaml:"token"`
	AllowedUsers    []string `yaml:"allowed_users"`
	BannedUsers     []string `yaml:"banned_users"`
	RequireMention  *bool    `yaml:"require_mention"`
	ServerURL       string   `yaml:"server_url"`
	ServerPassword  *string  `yaml:"server_password"`
	WebhookHost     string   `yaml:"webhook_host"`
	WebhookPort     int      `yaml:"webhook_port"`
	WebhookPath     string   `yaml:"webhook_path"`
	MentionPatterns []string `yaml:"mention_pattern"`
	APIHost         string   `yaml:"api_host"`
	APIPort         int      `yaml:"api_port"`
}
type platformYAML struct {
	Enabled bool              `yaml:"enabled"`
	Extra   platformExtraYAML `yaml:"extra"`
}
type documentYAML struct {
	Providers map[string]providerYAML `yaml:"providers"`
	Model     modelYAML               `yaml:"model"`
	Gateway   *struct {
		MultiplexProfiles bool           `yaml:"multiplex_profiles"`
		Routes            []ProfileRoute `yaml:"profile_routes"`
	} `yaml:"gateway"`
	Platforms map[string]platformYAML `yaml:"platforms"`
	Tools     toolsYAML               `yaml:"tools"`
	Runtime   *struct {
		LogLevel string `yaml:"log_level"`
	} `yaml:"runtime"`
	MCP struct {
		Servers map[string]mcpServerYAML `yaml:"servers"`
	} `yaml:"mcp"`
}

type mcpServerYAML struct {
	URL         string            `yaml:"url"`
	Description string            `yaml:"description"`
	Transport   string            `yaml:"transport"`
	Headers     map[string]string `yaml:"headers"`
	Enabled     *bool             `yaml:"enabled"`
}

// MCPServer holds one resolved operator-owned profile connection. It is never
// persisted in SQLite, and its URL/headers must not be logged.
type MCPServer struct {
	Name, URL, Description, Transport string
	Headers                           map[string]string
	Enabled                           bool
}

var profileNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidProfileName excludes traversal, hidden entries, and the reserved default name.
func ValidProfileName(name string) bool { return name != "default" && profileNameRE.MatchString(name) }

func privateDirectory(path, source string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return withConfigSource(configErr("config_dir_unavailable", ""), source)
	}
	current := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return withConfigSource(configErr("config_dir_unavailable", ""), source)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (current == abs && info.Mode().Perm()&0077 != 0) {
			return withConfigSource(configErr("config_dir_unsafe", ""), source)
		}
	}
	return nil
}

// knownPlatforms bounds configured platform names before any validation.
var knownPlatforms = map[string]bool{"discord": true, "bluebubbles": true, "api": true}

var platformTopKeys = map[string]bool{"enabled": true, "extra": true}

var platformExtraKeys = map[string]bool{
	"token": true, "allowed_users": true, "banned_users": true,
	"require_mention": true,
	"server_url":      true, "server_password": true,
	"webhook_host": true, "webhook_port": true, "webhook_path": true, "mention_pattern": true,
	"api_host": true, "api_port": true,
}

func mappingValueIndex(node *yaml.Node, key string) int {
	if node == nil || node.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return i + 1
		}
	}
	return -1
}

// platformEnabled resolves only a platform's enabled flag. An unset variable
// gating the flag leaves the platform disabled rather than failing the load.
func platformEnabled(name string, node *yaml.Node, env profileEnvironment) (bool, error) {
	index := mappingValueIndex(node, "enabled")
	if index < 0 {
		return false, nil
	}
	value := node.Content[index]
	path := "platforms." + name + ".enabled"
	if value.Kind != yaml.ScalarNode {
		return false, configErr("config_platform_enabled_invalid", path)
	}
	switch value.Tag {
	case "!!bool":
		return value.Value == "true", nil
	case "!!str":
		resolved, err := env.interpolateOptional(value.Value)
		if err != nil {
			return false, withConfigPath(err, path)
		}
		switch strings.TrimSpace(resolved) {
		case "", "false":
			return false, nil
		case "true":
			return true, nil
		}
	}
	return false, configErr("config_platform_enabled_invalid", path)
}

// validatePlatformKeys rejects unknown or duplicate keys without resolving any
// values, so a disabled platform can still report a typo.
func validatePlatformKeys(name string, node *yaml.Node) error {
	path := "platforms." + name
	if node.Kind != yaml.MappingNode {
		return configErr("config_key_invalid", path)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !platformTopKeys[key] {
			return configErr("config_key_unknown", path+"."+key)
		}
		if key != "extra" {
			continue
		}
		extra := node.Content[i+1]
		if extra.Kind != yaml.MappingNode {
			return configErr("config_key_invalid", path+".extra")
		}
		for j := 0; j+1 < len(extra.Content); j += 2 {
			if field := extra.Content[j].Value; !platformExtraKeys[field] {
				return configErr("config_key_unknown", path+".extra."+field)
			}
		}
	}
	return nil
}

// pruneDisabledPlatforms drops disabled platform subtrees before strict
// interpolation. Operators do not need credentials for platforms turned off,
// while unknown keys and unsupported platform names are still rejected.
func pruneDisabledPlatforms(raw *yaml.Node, env profileEnvironment) (*yaml.Node, error) {
	pruned := cloneYAML(raw)
	platforms := mappingValueIndex(pruned, "platforms")
	if platforms < 0 {
		return pruned, nil
	}
	node := pruned.Content[platforms]
	if node.Kind != yaml.MappingNode {
		return nil, configErr("config_key_invalid", "platforms")
	}
	kept := make([]*yaml.Node, 0, len(node.Content))
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		entry := node.Content[i+1]
		if !knownPlatforms[name] {
			return nil, configErr("config_platform_unsupported", "platforms."+name)
		}
		if err := validatePlatformKeys(name, entry); err != nil {
			return nil, err
		}
		enabled, err := platformEnabled(name, entry, env)
		if err != nil {
			return nil, err
		}
		if enabled {
			kept = append(kept, node.Content[i], node.Content[i+1])
		}
	}
	node.Content = kept
	return pruned, nil
}

func applyDocument(cfg *Config, doc documentYAML, providers map[string]providerYAML, selected *string) error {
	for name, value := range doc.Providers {
		providers[name] = value
	}
	if doc.Model.Provider != nil {
		*selected = *doc.Model.Provider
	}
	if doc.Model.Default != nil {
		cfg.LLMGatewayModel = *doc.Model.Default
	}
	if doc.Model.ContextLength != nil {
		cfg.ModelContextWindow = *doc.Model.ContextLength
	}
	if !strings.HasPrefix(*selected, "custom:") {
		return configErr("config_model_provider_invalid", "model.provider")
	}
	providerName := strings.TrimPrefix(*selected, "custom:")
	provider, ok := providers[providerName]
	if !ok {
		return configErr("config_model_provider_invalid", "model.provider")
	}
	endpoint, err := url.Parse(provider.API)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return configErr("config_model_endpoint_invalid", "providers."+providerName+".api")
	}
	// The LLM transport appends /v1 routes itself.
	endpoint.Path = strings.TrimSuffix(strings.TrimRight(endpoint.Path, "/"), "/v1")
	cfg.LLMGatewayURL = endpoint.String()
	cfg.LLMGatewayAPIKey, err = credential(provider.Key, provider.Key != nil)
	if err != nil {
		return withConfigPath(err, "providers."+providerName+".key")
	}
	if strings.TrimSpace(cfg.LLMGatewayModel) == "" || cfg.ModelContextWindow < 0 {
		return configErr("config_model_invalid", "model")
	}
	if doc.Tools.WebSearch.BraveKey != nil {
		cfg.BraveAPIKey, err = credential(doc.Tools.WebSearch.BraveKey, false)
		if err != nil {
			return withConfigPath(err, "tools.web_search.brave_key")
		}
	}
	if doc.Tools.WebSearch.SearxngURL != nil {
		cfg.SearxngURL = *doc.Tools.WebSearch.SearxngURL
	}
	if doc.Tools.ImageGenerate.URL != nil {
		cfg.ComfyUIURL = *doc.Tools.ImageGenerate.URL
	}
	if doc.Tools.ImageGenerate.Timeout != nil {
		cfg.ComfyUIGenerationTimeout, err = time.ParseDuration(*doc.Tools.ImageGenerate.Timeout)
		if err != nil || cfg.ComfyUIGenerationTimeout <= 0 {
			return configErr("config_tool_timeout_invalid", "tools.image_generate.generation_timeout")
		}
	}
	for _, entry := range []struct{ raw, path string }{{cfg.ComfyUIURL, "tools.image_generate.url"}, {cfg.SearxngURL, "tools.web_search.searxng_url"}} {
		if entry.raw == "" {
			continue
		}
		endpoint, err := url.Parse(entry.raw)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return configErr("config_tool_endpoint_invalid", entry.path)
		}
	}
	cfg.MCPServers = nil
	if len(doc.MCP.Servers) > 32 {
		return configErr("config_mcp_invalid", "mcp.servers")
	}
	for name, server := range doc.MCP.Servers {
		base := "mcp.servers." + name
		if !regexp.MustCompile(`^[a-z][a-z0-9_]{1,39}$`).MatchString(name) || name == "soul" {
			return configErr("config_mcp_invalid", base)
		}
		transport := server.Transport
		if transport == "" {
			transport = "streamable_http"
		}
		if transport != "streamable_http" {
			return configErr("config_mcp_invalid", base+".transport")
		}
		endpoint, err := url.Parse(server.URL)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
			return configErr("config_mcp_invalid", base+".url")
		}
		description := strings.TrimSpace(server.Description)
		if description == "" || len([]rune(description)) > 500 || strings.ContainsAny(description, "\r\n\x00") {
			return configErr("config_mcp_invalid", base+".description")
		}
		if len(server.Headers) > 32 {
			return configErr("config_mcp_invalid", base+".headers")
		}
		for key, value := range server.Headers {
			if !regexp.MustCompile(`^[!#$%&'*+.^_`+"`"+`|~0-9A-Za-z-]+$`).MatchString(key) || len(value) > 8192 || strings.ContainsAny(value, "\r\n\x00") {
				return configErr("config_mcp_invalid", base+".headers")
			}
		}
		enabled := true
		if server.Enabled != nil {
			enabled = *server.Enabled
		}
		cfg.MCPServers = append(cfg.MCPServers, MCPServer{Name: name, URL: server.URL, Description: description, Transport: transport, Headers: server.Headers, Enabled: enabled})
	}
	sort.Slice(cfg.MCPServers, func(i, j int) bool { return cfg.MCPServers[i].Name < cfg.MCPServers[j].Name })
	return nil
}

func port(value int) (string, error) {
	if value < 1 || value > 65535 {
		return "", configErr("config_port_invalid", "")
	}
	return strconv.Itoa(value), nil
}

// LoadProfiles merges raw YAML before resolving each profile's private .env and
// process environment. Named profiles inherit the default .env and override it
// with their own. It never mutates os.Environ; errors omit private content.
func LoadProfiles(root string) (*Config, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, withConfigSource(configErr("config_dir_unavailable", ""), "global_root")
	}
	if err := privateDirectory(root, "global_root"); err != nil {
		return nil, err
	}
	raw, err := readYAMLNode(filepath.Join(root, "config.yaml"), false)
	if err != nil {
		return nil, withConfigSource(err, "global_config")
	}
	env, err := loadProfileEnvironment(root, "global_env")
	if err != nil {
		return nil, err
	}
	// Disabled platforms are dropped before interpolation so their credentials
	// are never required. Their keys are still validated above.
	pruned, err := pruneDisabledPlatforms(raw, env)
	if err != nil {
		return nil, err
	}
	doc, err := resolveDocument(pruned, env)
	if err != nil {
		return nil, err
	}
	cfg := &Config{ProfileRoot: root, ProfileName: "default", Profiles: map[string]*Config{}, LogLevel: LevelInfo, ComfyUIGenerationTimeout: 2 * time.Minute, DiscordGroupRequireMention: true, BlueBubblesGroupRequireMention: true}
	providers := map[string]providerYAML{}
	selected := ""
	if err := applyDocument(cfg, doc, providers, &selected); err != nil {
		return nil, err
	}
	if doc.Runtime != nil {
		cfg.LogLevel = ParseLevel(doc.Runtime.LogLevel)
	}
	names := map[string]bool{}
	if doc.Gateway != nil {
		seen := map[string]bool{}
		for _, route := range doc.Gateway.Routes {
			if route.Profile != "default" && !ValidProfileName(route.Profile) {
				return nil, configErr("config_route_invalid", "gateway.profile_routes")
			}
			if route.Profile != "default" && !doc.Gateway.MultiplexProfiles {
				return nil, configErr("config_route_invalid", "gateway.multiplex_profiles")
			}
			if route.Platform == "bluebubbles" {
				route.Platform = "imessage"
			}
			if route.Platform != "discord" && route.Platform != "imessage" {
				return nil, configErr("config_route_invalid", "gateway.profile_routes")
			}
			id, err := NormalizeGatewayIdentifier(route.Platform, route.UserID)
			if err != nil {
				return nil, configErr("config_route_invalid", "gateway.profile_routes")
			}
			route.UserID = id
			key := route.Platform + ":" + id
			if seen[key] {
				return nil, configErr("config_route_invalid", "gateway.profile_routes")
			}
			seen[key] = true
			cfg.ProfileRoutes = append(cfg.ProfileRoutes, route)
			if route.Profile != "default" {
				names[route.Profile] = true
			}
		}
	}
	for platform, settings := range doc.Platforms {
		if !knownPlatforms[platform] {
			return nil, configErr("config_platform_unsupported", "platforms."+platform)
		}
		if !settings.Enabled {
			continue
		}
		base := "platforms." + platform + ".extra."
		x := settings.Extra
		switch platform {
		case "discord":
			cfg.DiscordToken, err = credential(x.Token, true)
			if err != nil {
				return nil, withConfigPath(err, base+"token")
			}
			cfg.DiscordPolicy, err = admissionPolicy("discord", base, x.AllowedUsers, x.BannedUsers)
			if err != nil {
				return nil, err
			}
			if x.RequireMention != nil {
				cfg.DiscordGroupRequireMention = *x.RequireMention
			}
		case "bluebubbles":
			cfg.BlueBubblesURL = x.ServerURL
			endpoint, parseErr := url.Parse(x.ServerURL)
			if parseErr != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
				return nil, configErr("config_endpoint_invalid", base+"server_url")
			}
			if x.WebhookHost != "" && x.WebhookHost != "0.0.0.0" {
				return nil, configErr("config_platform_host_unsupported", base+"webhook_host")
			}
			cfg.BlueBubblesListenPort, err = port(x.WebhookPort)
			if err != nil {
				return nil, withConfigPath(err, base+"webhook_port")
			}
			cfg.BlueBubblesPassword, err = credential(x.ServerPassword, true)
			if err != nil {
				return nil, withConfigPath(err, base+"server_password")
			}
			cfg.BlueBubblesPolicy, err = admissionPolicy("imessage", base, x.AllowedUsers, x.BannedUsers)
			if err != nil {
				return nil, err
			}
			if x.RequireMention != nil {
				cfg.BlueBubblesGroupRequireMention = *x.RequireMention
			}
			cfg.BlueBubblesWebhookPath, err = parseWebhookPath(x.WebhookPath)
			if err != nil {
				return nil, withConfigPath(err, base+"webhook_path")
			}
			cfg.BlueBubblesMentionPatterns, err = compileMentionPatterns(x.MentionPatterns, base+"mention_pattern")
			if err != nil {
				return nil, err
			}
		case "api":
			if x.APIHost != "" && x.APIHost != "127.0.0.1" {
				return nil, configErr("config_platform_host_unsupported", base+"api_host")
			}
			cfg.OpenAIListenPort, err = port(x.APIPort)
			if err != nil {
				return nil, withConfigPath(err, base+"api_port")
			}
		}
	}
	if cfg.DiscordToken == "" && cfg.BlueBubblesListenPort == "" && cfg.OpenAIListenPort == "" {
		return nil, configErr("config_gateway_unavailable", "")
	}
	for name := range names {
		profileRoot := filepath.Join(root, "profiles", name)
		if err := privateDirectory(profileRoot, "profile_root"); err != nil {
			return nil, err
		}
		profileRaw, err := readYAMLNode(filepath.Join(profileRoot, "config.yaml"), true)
		if err != nil {
			return nil, withConfigSource(err, "profile_config")
		}
		merged, err := profileDocument(raw, profileRaw)
		if err != nil {
			return nil, err
		}
		ownEnv, err := loadProfileEnvironment(profileRoot, "profile_env")
		if err != nil {
			return nil, err
		}
		profileEnv := mergeEnvironments(env, ownEnv)
		profileDoc, err := resolveDocument(merged, profileEnv)
		if err != nil {
			return nil, err
		}
		profile := *cfg
		profile.ProfileName, profile.ProfileRoot, profile.Profiles = name, profileRoot, nil
		profileProviders := map[string]providerYAML{}
		profileSelected := ""
		if err := applyDocument(&profile, profileDoc, profileProviders, &profileSelected); err != nil {
			return nil, err
		}
		cfg.Profiles[name] = &profile
	}
	return cfg, nil
}

// admissionPolicy normalizes allow/ban identities for one platform. An empty
// allowed list admits every routed identity; banned identities always win.
func admissionPolicy(platform, base string, allowed, banned []string) (AdmissionPolicy, error) {
	allowedIDs, err := normalizeIdentities(platform, allowed)
	if err != nil {
		return AdmissionPolicy{}, configErr("config_value_invalid", base+"allowed_users")
	}
	bannedIDs, err := normalizeIdentities(platform, banned)
	if err != nil {
		return AdmissionPolicy{}, configErr("config_value_invalid", base+"banned_users")
	}
	return AdmissionPolicy{Allowed: allowedIDs, Banned: bannedIDs}, nil
}

// normalizeIdentities canonicalizes and deduplicates configured identities.
func normalizeIdentities(platform string, values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		id, err := NormalizeGatewayIdentifier(platform, value)
		if err != nil {
			return nil, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		normalized = append(normalized, id)
	}
	return normalized, nil
}

// parseWebhookPath validates an operator-supplied BlueBubbles webhook route.
func parseWebhookPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultBlueBubblesWebhookPath, nil
	}
	if !webhookPathRE.MatchString(value) || strings.Contains(value, "..") {
		return "", configErr("config_value_invalid", "")
	}
	return value, nil
}

// compileMentionPatterns validates and anchors configured mention patterns. A
// mention must begin the message, so every pattern is compiled with a leading
// anchor; patterns that would match the empty string are rejected.
func compileMentionPatterns(patterns []string, path string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	if len(patterns) > maxMentionPatterns {
		return nil, configErr("config_value_invalid", path)
	}
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern == "" || len([]rune(pattern)) > maxMentionPatternRunes || strings.ContainsAny(pattern, "\x00\r\n") {
			return nil, configErr("config_value_invalid", path)
		}
		anchored, err := regexp.Compile("^(?:" + pattern + ")")
		if err != nil || anchored.MatchString("") {
			return nil, configErr("config_value_invalid", path)
		}
		compiled = append(compiled, anchored)
	}
	return compiled, nil
}

// NormalizeGatewayIdentifier canonicalizes configured and authenticated identities.
func NormalizeGatewayIdentifier(platform, value string) (string, error) {
	value = strings.TrimSpace(value)
	switch platform {
	case "discord":
		if !regexp.MustCompile(`^[0-9]+$`).MatchString(value) {
			return "", errors.New("invalid Discord identity")
		}
	case "imessage":
		if strings.Contains(value, "@") {
			value = strings.ToLower(value)
			if strings.ContainsAny(value, " \t\r\n") {
				return "", errors.New("invalid iMessage identity")
			}
		} else {
			value = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(value)
			if strings.HasPrefix(value, "00") {
				value = "+" + value[2:]
			}
			if !strings.HasPrefix(value, "+") {
				value = "+" + value
			}
			if !regexp.MustCompile(`^\+[0-9]{7,15}$`).MatchString(value) {
				return "", errors.New("invalid iMessage identity")
			}
		}
	default:
		return "", fmt.Errorf("unsupported identity platform")
	}
	return value, nil
}
