package config

import (
	"regexp"
	"time"
)

// Config holds validated effective profile settings and shared gateway policy.
type Config struct {
	MCPServers                     []MCPServer
	ProfileRoot                    string
	ProfileName                    string
	Profiles                       map[string]*Config
	ProfileRoutes                  []ProfileRoute
	DiscordPolicy                  AdmissionPolicy
	BlueBubblesPolicy              AdmissionPolicy
	DiscordGroupRequireMention     bool
	BlueBubblesGroupRequireMention bool
	BlueBubblesListenPort          string
	BlueBubblesURL                 string
	BlueBubblesPassword            string
	BlueBubblesWebhookPath         string
	// BlueBubblesWebhookCallbackHost overrides auto-detected callback
	// address registration. Empty preserves UDP-dial interface detection.
	BlueBubblesWebhookCallbackHost string
	BlueBubblesMentionPatterns     []*regexp.Regexp
	DiscordToken                   string
	OpenAIListenHost               string
	OpenAIListenPort               string
	// OpenAIAPIKey is the per-profile bearer credential for the OpenAI gateway.
	// Empty means the profile has no API access. It must never be logged.
	OpenAIAPIKey             string
	LLMGatewayURL            string
	LLMGatewayModel          string
	LLMGatewayProvider       string
	LLMGatewayAPIKey         string
	LLMGatewayVirtualKey     string
	ModelContextWindow       int
	BraveAPIKey              string
	SearxngURL               string
	ComfyUIURL               string
	ComfyUIGenerationTimeout time.Duration
	// Browser backend configuration for the local headless browser_* tools.
	// Empty launch command and endpoint URL disable the tools. Bounds must
	// never be logged; only counts and durations are model-visible.
	BrowserLaunchCommand     string
	BrowserEndpointURL       string
	BrowserCommandTimeout    time.Duration
	BrowserSnapshotThreshold int
	BrowserInactivityTimeout time.Duration
	BrowserRecordSessions    bool
	BrowserAllowPrivateURLs  bool
	// BrowserAllowInsecureURLs opts into plaintext http page loads. Default
	// false (https only). Other schemes stay blocked regardless.
	BrowserAllowInsecureURLs bool
	// BrowserRestrictEvaluate opts into the sensitive-primitive eval
	// denylist (cookie/storage reads, network emission, form-value
	// extraction). Default false: the denylist gates on primitive names
	// and also blocks legitimate DOM extraction.
	BrowserRestrictEvaluate bool
	// SkillsSourceDir is the optional external skill-library directory,
	// resolved absolute (relative paths anchor at the profile root). Empty
	// disables the external tier. It must never be logged.
	SkillsSourceDir string
	LogLevel        Level
}

// RetentionPolicy supplies code-owned worker bounds.
type RetentionPolicy struct {
	SessionInactivity      time.Duration
	PendingDeliveryTimeout time.Duration
	MaintenanceInterval    time.Duration
	BatchSize              int
}

// DefaultRetentionPolicy returns fixed retention and maintenance bounds.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{SessionInactivity: 24 * time.Hour, PendingDeliveryTimeout: 15 * time.Minute, MaintenanceInterval: time.Hour, BatchSize: 100}
}

// OswaldHomeDir is the home directory holding global configuration and the
// default profile.
const OswaldHomeDir = ".oswald"

// Load reads global/profile YAML using isolated profile environments.
func Load() (*Config, error) { return LoadProfiles(OswaldHomeDir) }
