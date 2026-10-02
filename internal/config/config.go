package config

import "time"

// Config holds validated effective profile settings and shared gateway policy.
type Config struct {
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
	BlueBubblesDMMention           bool
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
	RetiredIndexRetention    time.Duration
	SessionInactivity        time.Duration
	PendingDeliveryTimeout   time.Duration
	SuccessfulJobRetention   time.Duration
	DeadJobRetention         time.Duration
	AccountChallengeGrace    time.Duration
	MaintenanceInterval      time.Duration
	DatabaseOptimizeInterval time.Duration
	BatchSize                int
}

// DefaultRetentionPolicy returns fixed retention and maintenance bounds.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{RetiredIndexRetention: 168 * time.Hour, SessionInactivity: 24 * time.Hour, PendingDeliveryTimeout: 15 * time.Minute, SuccessfulJobRetention: 168 * time.Hour, DeadJobRetention: 720 * time.Hour, AccountChallengeGrace: 24 * time.Hour, MaintenanceInterval: time.Hour, DatabaseOptimizeInterval: 24 * time.Hour, BatchSize: 100}
}

// DefaultDataRoot is the default profile and global configuration directory.
const DefaultDataRoot = ".oswald"

// Load reads global/profile YAML and never loads dotenv files.
func Load() (*Config, error) { return LoadProfiles(DefaultDataRoot) }
