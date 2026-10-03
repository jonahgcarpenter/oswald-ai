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
	HomeAssistantListenPort        string
	HomeAssistantAuthToken         string
	BlueBubblesListenPort          string
	BlueBubblesURL                 string
	BlueBubblesPassword            string
	BlueBubblesWebhookPath         string
	BlueBubblesMentionPatterns     []*regexp.Regexp
	DiscordToken                   string
	OpenAIListenPort               string
	LLMGatewayURL                  string
	LLMGatewayModel                string
	LLMGatewayAPIKey               string
	LLMGatewayVirtualKey           string
	ModelContextWindow             int
	BraveAPIKey                    string
	SearxngURL                     string
	ComfyUIURL                     string
	ComfyUIGenerationTimeout       time.Duration
	WorkerPoolSize                 int
	LogLevel                       Level
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
