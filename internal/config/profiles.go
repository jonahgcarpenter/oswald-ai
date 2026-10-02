package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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

// AdmissionPolicy controls direct-message admission before expensive processing.
type AdmissionPolicy struct {
	Mode      string
	AllowFrom []string
}

// Allows reports whether an already normalized external identity is admitted.
func (p AdmissionPolicy) Allows(identifier string) bool {
	if p.Mode == "allow" {
		return true
	}
	for _, allowed := range p.AllowFrom {
		if allowed == identifier {
			return true
		}
	}
	return false
}

type providerYAML struct {
	API    string `yaml:"api"`
	KeyEnv string `yaml:"key_env"`
}
type modelYAML struct {
	Provider      *string `yaml:"provider"`
	Default       *string `yaml:"default"`
	ContextLength *int    `yaml:"context_length"`
}
type webYAML struct {
	BraveKeyEnv *string `yaml:"brave_key_env"`
	SearxngURL  *string `yaml:"searxng_url"`
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
	TokenEnv            string   `yaml:"token_env"`
	DMPolicy            string   `yaml:"dm_policy"`
	AllowFrom           []string `yaml:"allow_from"`
	GuildRequireMention *bool    `yaml:"guild_require_mention"`
	GroupRequireMention *bool    `yaml:"group_require_mention"`
	ServerURL           string   `yaml:"server_url"`
	ServerPasswordEnv   string   `yaml:"server_password_env"`
	WebhookHost         string   `yaml:"webhook_host"`
	WebhookPort         int      `yaml:"webhook_port"`
	APIHost             string   `yaml:"api_host"`
	APIPort             int      `yaml:"api_port"`
	AuthTokenEnv        string   `yaml:"auth_token_env"`
	ListenPort          int      `yaml:"listen_port"`
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
		Workers  int    `yaml:"worker_pool_size"`
		LogLevel string `yaml:"log_level"`
	} `yaml:"runtime"`
	// MCP is reserved operator data; no remote MCP wiring is enabled yet.
	MCP yaml.Node `yaml:"mcp"`
}

var profileNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidProfileName excludes traversal, hidden entries, and the reserved default name.
func ValidProfileName(name string) bool { return name != "default" && profileNameRE.MatchString(name) }

func privateDirectory(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return errors.New("profile directory is unavailable")
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (current == abs && info.Mode().Perm()&0077 != 0) {
			return errors.New("unsafe profile directory")
		}
	}
	return nil
}

func readYAML(path string, optional bool) (documentYAML, error) {
	var doc documentYAML
	info, err := os.Lstat(path)
	if optional && errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, errors.New("configuration file is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 256*1024 {
		return doc, errors.New("unsafe or oversized configuration file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return doc, errors.New("configuration file is unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() > 256*1024 {
		return doc, errors.New("unsafe or oversized configuration file")
	}
	decoder := yaml.NewDecoder(io.LimitReader(f, 256*1024+1))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, errors.New("invalid configuration YAML")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return doc, errors.New("configuration must contain exactly one YAML document")
	}
	return doc, nil
}

func secret(name string, required bool) (string, error) {
	if name == "" {
		if required {
			return "", errors.New("required credential reference is missing")
		}
		return "", nil
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(name) {
		return "", errors.New("invalid credential environment reference")
	}
	value := os.Getenv(name)
	if required && strings.TrimSpace(value) == "" {
		return "", errors.New("required process credential is missing")
	}
	return value, nil
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
		return errors.New("model provider must reference custom:<provider>")
	}
	provider, ok := providers[strings.TrimPrefix(*selected, "custom:")]
	if !ok {
		return errors.New("selected model provider is undefined")
	}
	endpoint, err := url.Parse(provider.API)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("invalid model provider endpoint")
	}
	// The LLM transport appends /v1 routes itself.
	endpoint.Path = strings.TrimSuffix(strings.TrimRight(endpoint.Path, "/"), "/v1")
	cfg.LLMGatewayURL = endpoint.String()
	cfg.LLMGatewayAPIKey, err = secret(provider.KeyEnv, provider.KeyEnv != "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.LLMGatewayModel) == "" || cfg.ModelContextWindow < 0 {
		return errors.New("invalid model configuration")
	}
	if doc.Tools.WebSearch.BraveKeyEnv != nil {
		cfg.BraveAPIKey, err = secret(*doc.Tools.WebSearch.BraveKeyEnv, *doc.Tools.WebSearch.BraveKeyEnv != "")
		if err != nil {
			return err
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
			return errors.New("invalid image generation timeout")
		}
	}
	for _, raw := range []string{cfg.ComfyUIURL, cfg.SearxngURL} {
		if raw == "" {
			continue
		}
		endpoint, err := url.Parse(raw)
		if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return errors.New("invalid tool provider endpoint")
		}
	}
	return nil
}

func port(value int) (string, error) {
	if value < 1 || value > 65535 {
		return "", errors.New("invalid listener port")
	}
	return strconv.Itoa(value), nil
}

// LoadProfiles loads global YAML and manually provisioned named profiles. It never
// reads .env files or changes the process environment; errors omit private YAML.
func LoadProfiles(root string) (*Config, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := privateDirectory(root); err != nil {
		return nil, err
	}
	doc, err := readYAML(filepath.Join(root, "config.yaml"), false)
	if err != nil {
		return nil, err
	}
	cfg := &Config{ProfileRoot: root, ProfileName: "default", Profiles: map[string]*Config{}, WorkerPoolSize: 1, LogLevel: LevelInfo, ComfyUIGenerationTimeout: 2 * time.Minute, DiscordGroupRequireMention: true, BlueBubblesGroupRequireMention: true}
	providers := map[string]providerYAML{}
	selected := ""
	if err := applyDocument(cfg, doc, providers, &selected); err != nil {
		return nil, err
	}
	if doc.Runtime != nil {
		cfg.WorkerPoolSize = doc.Runtime.Workers
		if cfg.WorkerPoolSize <= 0 {
			return nil, errors.New("worker pool must be positive")
		}
		cfg.LogLevel = ParseLevel(doc.Runtime.LogLevel)
	}
	names := map[string]bool{}
	if doc.Gateway != nil {
		seen := map[string]bool{}
		for _, route := range doc.Gateway.Routes {
			if route.Profile != "default" && !ValidProfileName(route.Profile) {
				return nil, errors.New("invalid routed profile name")
			}
			if route.Profile != "default" && !doc.Gateway.MultiplexProfiles {
				return nil, errors.New("named profile routes require multiplex_profiles")
			}
			if route.Platform == "bluebubbles" {
				route.Platform = "imessage"
			}
			if route.Platform != "discord" && route.Platform != "imessage" && route.Platform != "homeassistant" {
				return nil, errors.New("unsupported profile route platform")
			}
			id, err := NormalizeGatewayIdentifier(route.Platform, route.UserID)
			if err != nil {
				return nil, err
			}
			route.UserID = id
			key := route.Platform + ":" + id
			if seen[key] {
				return nil, errors.New("conflicting profile routes")
			}
			seen[key] = true
			cfg.ProfileRoutes = append(cfg.ProfileRoutes, route)
			if route.Profile != "default" {
				names[route.Profile] = true
			}
		}
	}
	for platform, settings := range doc.Platforms {
		if platform != "discord" && platform != "bluebubbles" && platform != "api" && platform != "homeassistant" {
			return nil, errors.New("unsupported gateway platform")
		}
		if !settings.Enabled {
			continue
		}
		x := settings.Extra
		switch platform {
		case "discord":
			cfg.DiscordToken, err = secret(x.TokenEnv, true)
			cfg.DiscordPolicy, err = admissionPolicy("discord", x.DMPolicy, x.AllowFrom, err)
			if x.GuildRequireMention != nil {
				cfg.DiscordGroupRequireMention = *x.GuildRequireMention
			}
		case "bluebubbles":
			cfg.BlueBubblesURL = x.ServerURL
			endpoint, parseErr := url.Parse(x.ServerURL)
			if parseErr != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
				return nil, errors.New("invalid BlueBubbles endpoint")
			}
			if x.WebhookHost != "" && x.WebhookHost != "0.0.0.0" {
				return nil, errors.New("unsupported webhook host")
			}
			cfg.BlueBubblesListenPort, err = port(x.WebhookPort)
			if err != nil {
				return nil, err
			}
			cfg.BlueBubblesPassword, err = secret(x.ServerPasswordEnv, true)
			cfg.BlueBubblesPolicy, err = admissionPolicy("imessage", x.DMPolicy, x.AllowFrom, err)
			if x.GroupRequireMention != nil {
				cfg.BlueBubblesGroupRequireMention = *x.GroupRequireMention
			}
		case "api":
			if x.APIHost != "" && x.APIHost != "127.0.0.1" {
				return nil, errors.New("API listener must remain loopback-only")
			}
			cfg.OpenAIListenPort, err = port(x.APIPort)
			names["api"] = true
		case "homeassistant":
			cfg.HomeAssistantListenPort, err = port(x.ListenPort)
			if err != nil {
				return nil, err
			}
			cfg.HomeAssistantAuthToken, err = secret(x.AuthTokenEnv, true)
			if len(strings.TrimSpace(cfg.HomeAssistantAuthToken)) < 32 {
				return nil, errors.New("home assistant credential is too short")
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if cfg.DiscordToken == "" && cfg.BlueBubblesListenPort == "" && cfg.OpenAIListenPort == "" && cfg.HomeAssistantListenPort == "" {
		return nil, errors.New("no gateways are enabled")
	}
	for name := range names {
		profileRoot := filepath.Join(root, "profiles", name)
		if err := privateDirectory(profileRoot); err != nil {
			return nil, err
		}
		profileDoc, err := readYAML(filepath.Join(profileRoot, "config.yaml"), true)
		if err != nil {
			return nil, err
		}
		if profileDoc.Gateway != nil || profileDoc.Platforms != nil || profileDoc.Runtime != nil {
			return nil, errors.New("profile cannot override shared gateway or runtime configuration")
		}
		profile := *cfg
		profile.ProfileName, profile.ProfileRoot, profile.Profiles = name, profileRoot, nil
		profileProviders := map[string]providerYAML{}
		for k, v := range providers {
			profileProviders[k] = v
		}
		profileSelected := selected
		if err := applyDocument(&profile, profileDoc, profileProviders, &profileSelected); err != nil {
			return nil, err
		}
		cfg.Profiles[name] = &profile
	}
	return cfg, nil
}

func admissionPolicy(platform, mode string, identifiers []string, prior error) (AdmissionPolicy, error) {
	if prior != nil {
		return AdmissionPolicy{}, prior
	}
	if mode == "" {
		mode = "allowlist"
	}
	if mode != "allowlist" && mode != "allow" {
		return AdmissionPolicy{}, errors.New("unsupported DM policy")
	}
	policy := AdmissionPolicy{Mode: mode}
	for _, id := range identifiers {
		normalized, err := NormalizeGatewayIdentifier(platform, id)
		if err != nil {
			return AdmissionPolicy{}, err
		}
		policy.AllowFrom = append(policy.AllowFrom, normalized)
	}
	return policy, nil
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
	case "homeassistant":
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(value) {
			return "", errors.New("invalid Home Assistant identity")
		}
	default:
		return "", fmt.Errorf("unsupported identity platform")
	}
	return value, nil
}
