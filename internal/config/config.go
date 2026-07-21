package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	Port                              string          // HTTP port for the WebSocket gateway (default: "8000")
	WebSocketAuthSigningKey           string          // Raw or base64-encoded HMAC key used to sign WebSocket access tokens
	WebSocketAuthMaxTokenTTL          time.Duration   // WebSocket access-token lifetime, capped at 15 minutes (default: 15m)
	IMessagePort                      string          // HTTP port for the BlueBubbles webhook listener (default: "8090")
	IMessageWebhookPath               string          // HTTP path for incoming BlueBubbles webhooks (default: "/imessage/webhook")
	BlueBubblesURL                    string          // BlueBubbles server base URL; iMessage gateway disabled if empty
	BlueBubblesPassword               string          // BlueBubbles server password/token for REST API auth
	MCPConfigEncryptionKey            string          // Key used to encrypt MCP server URLs and headers at rest
	LLMGatewayURL                     string          // LLM gateway API base URL (default: "http://localhost:8080")
	LLMGatewayModel                   string          // LLM gateway model name; required, startup fails if empty
	LLMGatewayEmbeddingModel          string          // Optional LLM gateway embedding model used for semantic durable-memory retrieval
	LLMGatewayAPIKey                  string          // Optional bearer token for LLM gateway requests
	LLMGatewayVirtualKey              string          // Optional gateway routing key for LLM gateway requests
	LLMGatewayTimeout                 time.Duration   // Expected upstream LLM gateway timeout; local guard timeouts are derived from it
	ModelContextWindow                int             // Optional model context-window override for prompt budgeting
	ModelMaxOutputTokens              int             // Optional model output-token reserve override for prompt budgeting
	DiscordToken                      string          // Discord bot token; Discord gateway disabled if empty
	SearxngURL                        string          // SearXNG base URL for web search (default: "http://localhost:8888")
	MaxToolFailureRetries             int             // Maximum consecutive tool execution failures before the agent stops retrying tools (default: 3)
	WorkerPoolSize                    int             // Number of concurrent broker workers (default: 1)
	BackgroundMemoryExtractionEnabled bool            // Enables automatic post-delivery memory extraction (default: true)
	LogLevel                          Level           // Logging verbosity (default: LevelInfo)
	RetentionPolicy                   RetentionPolicy // Memory retention and maintenance policy
}

// RetentionPolicy controls content expiry and periodic memory maintenance.
type RetentionPolicy struct {
	ForgottenContentGrace           time.Duration
	ContentBearingAuditJobRetention time.Duration
	ContentFreeTombstoneRetention   time.Duration
	RetiredIndexRetention           time.Duration
	SessionInactivity               time.Duration
	CandidateContentRetention       time.Duration
	SuccessfulJobRetention          time.Duration
	DeadJobRetention                time.Duration
	AccountChallengeGrace           time.Duration
	MaintenanceInterval             time.Duration
	DatabaseOptimizeInterval        time.Duration
	BatchSize                       int
}

const (
	DefaultSoulPath              = "data/memory/soul/soul.md"
	DefaultToolsConfigDir        = "data/tools"
	DefaultAccountLinkPath       = "data/database/oswald.db"
	DefaultLegacyAccountLinkPath = "data/accounts/links.json"
)

// Load reads configuration from environment variables, with .env file support.
// Missing variables use defaults; invalid security-sensitive values return an error.
func Load() (*Config, error) {
	// Silently ignore missing .env — production environments use real env vars
	godotenv.Load() // nolint: errcheck
	webSocketMaxTTL, err := getEnvDurationStrict("WEBSOCKET_AUTH_MAX_TOKEN_TTL", 15*time.Minute)
	if err != nil {
		return nil, err
	}
	retentionPolicy, err := loadRetentionPolicy()
	if err != nil {
		return nil, err
	}
	backgroundMemoryExtractionEnabled, err := getEnvBoolStrict("MEMORY_BACKGROUND_EXTRACTION_ENABLED", true)
	if err != nil {
		return nil, err
	}

	return &Config{
		Port:                              getEnv("PORT", "8000"),
		WebSocketAuthSigningKey:           getEnv("WEBSOCKET_AUTH_SIGNING_KEY", ""),
		WebSocketAuthMaxTokenTTL:          webSocketMaxTTL,
		IMessagePort:                      getEnv("IMESSAGE_PORT", "8090"),
		IMessageWebhookPath:               getEnv("IMESSAGE_WEBHOOK_PATH", "/imessage/webhook"),
		BlueBubblesURL:                    getEnv("BLUEBUBBLES_URL", ""),
		BlueBubblesPassword:               getEnv("BLUEBUBBLES_PASSWORD", ""),
		MCPConfigEncryptionKey:            getEnv("MCP_CONFIG_ENCRYPTION_KEY", ""),
		LLMGatewayURL:                     getEnv("LLM_GATEWAY_URL", "http://localhost:8080"),
		LLMGatewayModel:                   getEnv("LLM_GATEWAY_MODEL", ""),
		LLMGatewayEmbeddingModel:          getEnv("LLM_GATEWAY_EMBEDDING_MODEL", ""),
		LLMGatewayAPIKey:                  getEnv("LLM_GATEWAY_API_KEY", ""),
		LLMGatewayVirtualKey:              getEnv("LLM_GATEWAY_VIRTUAL_KEY", ""),
		LLMGatewayTimeout:                 getEnvDuration("LLM_GATEWAY_TIMEOUT", 180*time.Second),
		ModelContextWindow:                getEnvInt("MODEL_CONTEXT_WINDOW", 0),
		ModelMaxOutputTokens:              getEnvInt("MODEL_MAX_OUTPUT_TOKENS", 0),
		DiscordToken:                      getEnv("DISCORD_TOKEN", ""),
		SearxngURL:                        getEnv("SEARXNG_URL", "http://localhost:8080"),
		MaxToolFailureRetries:             getEnvInt("MAX_TOOL_FAILURE_RETRIES", 3),
		WorkerPoolSize:                    getEnvInt("WORKER_POOL_SIZE", 1),
		BackgroundMemoryExtractionEnabled: backgroundMemoryExtractionEnabled,
		LogLevel:                          ParseLevel(getEnv("LOG_LEVEL", "info")),
		RetentionPolicy:                   retentionPolicy,
	}, nil
}

func getEnvBoolStrict(key string, defaultValue bool) (bool, error) {
	value, exists := os.LookupEnv(key)
	if !exists {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return parsed, nil
}

func loadRetentionPolicy() (RetentionPolicy, error) {
	policy := RetentionPolicy{}
	durationValues := []struct {
		key          string
		defaultValue time.Duration
		destination  *time.Duration
	}{
		{key: "MEMORY_FORGOTTEN_CONTENT_GRACE", defaultValue: 720 * time.Hour, destination: &policy.ForgottenContentGrace},
		{key: "MEMORY_CONTENT_BEARING_AUDIT_JOB_RETENTION", defaultValue: 720 * time.Hour, destination: &policy.ContentBearingAuditJobRetention},
		{key: "MEMORY_CONTENT_FREE_TOMBSTONE_RETENTION", defaultValue: 8760 * time.Hour, destination: &policy.ContentFreeTombstoneRetention},
		{key: "MEMORY_RETIRED_INDEX_RETENTION", defaultValue: 168 * time.Hour, destination: &policy.RetiredIndexRetention},
		{key: "MEMORY_SESSION_INACTIVITY", defaultValue: 24 * time.Hour, destination: &policy.SessionInactivity},
		{key: "MEMORY_CANDIDATE_CONTENT_RETENTION", defaultValue: 720 * time.Hour, destination: &policy.CandidateContentRetention},
		{key: "MEMORY_SUCCESSFUL_JOB_RETENTION", defaultValue: 168 * time.Hour, destination: &policy.SuccessfulJobRetention},
		{key: "MEMORY_DEAD_JOB_RETENTION", defaultValue: 720 * time.Hour, destination: &policy.DeadJobRetention},
		{key: "MEMORY_ACCOUNT_CHALLENGE_GRACE", defaultValue: 24 * time.Hour, destination: &policy.AccountChallengeGrace},
		{key: "MEMORY_MAINTENANCE_INTERVAL", defaultValue: time.Hour, destination: &policy.MaintenanceInterval},
		{key: "MEMORY_DATABASE_OPTIMIZE_INTERVAL", defaultValue: 24 * time.Hour, destination: &policy.DatabaseOptimizeInterval},
	}

	for i := range durationValues {
		value, err := getEnvPositiveDuration(durationValues[i].key, durationValues[i].defaultValue)
		if err != nil {
			return RetentionPolicy{}, err
		}
		*durationValues[i].destination = value
	}

	batchSize, err := getEnvPositiveInt("MEMORY_MAINTENANCE_BATCH_SIZE", 100)
	if err != nil {
		return RetentionPolicy{}, err
	}
	policy.BatchSize = batchSize

	if policy.ContentFreeTombstoneRetention < policy.ContentBearingAuditJobRetention {
		return RetentionPolicy{}, fmt.Errorf("MEMORY_CONTENT_FREE_TOMBSTONE_RETENTION must be greater than or equal to MEMORY_CONTENT_BEARING_AUDIT_JOB_RETENTION")
	}
	if policy.DeadJobRetention < policy.SuccessfulJobRetention {
		return RetentionPolicy{}, fmt.Errorf("MEMORY_DEAD_JOB_RETENTION must be greater than or equal to MEMORY_SUCCESSFUL_JOB_RETENTION")
	}
	if policy.DatabaseOptimizeInterval < policy.MaintenanceInterval {
		return RetentionPolicy{}, fmt.Errorf("MEMORY_DATABASE_OPTIMIZE_INTERVAL must be greater than or equal to MEMORY_MAINTENANCE_INTERVAL")
	}

	return policy, nil
}

// getEnv retrieves an environment variable with a fallback to the default value
// if the variable is not set.
func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// getEnvInt retrieves an environment variable as an integer with a fallback default.
// Returns the default if the variable is missing or cannot be parsed as an integer.
func getEnvInt(key string, defaultValue int) int {
	value, exists := os.LookupEnv(key)
	if !exists || value == "" {
		return defaultValue
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return n
}

// getEnvDuration retrieves an environment variable as a Go duration string with a fallback default.
// Returns the default if the variable is missing or cannot be parsed as a duration.
func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	value, exists := os.LookupEnv(key)
	if !exists || value == "" {
		return defaultValue
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return defaultValue
	}
	return d
}

func getEnvDurationStrict(key string, defaultValue time.Duration) (time.Duration, error) {
	value, exists := os.LookupEnv(key)
	if !exists || value == "" {
		return defaultValue, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", key)
	}
	return d, nil
}

func getEnvPositiveDuration(key string, defaultValue time.Duration) (time.Duration, error) {
	value, exists := os.LookupEnv(key)
	if !exists {
		return defaultValue, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", key)
	}
	return d, nil
}

func getEnvPositiveInt(key string, defaultValue int) (int, error) {
	value, exists := os.LookupEnv(key)
	if !exists {
		return defaultValue, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}
