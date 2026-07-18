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
	Port                     string        // HTTP port for the WebSocket gateway (default: "8000")
	WebSocketAuthSigningKey  string        // Raw or base64-encoded HMAC key used to verify WebSocket identity tokens
	WebSocketAuthMaxTokenTTL time.Duration // Maximum accepted WebSocket identity token lifetime (default: 15m)
	IMessagePort             string        // HTTP port for the BlueBubbles webhook listener (default: "8090")
	IMessageWebhookPath      string        // HTTP path for incoming BlueBubbles webhooks (default: "/imessage/webhook")
	BlueBubblesURL           string        // BlueBubbles server base URL; iMessage gateway disabled if empty
	BlueBubblesPassword      string        // BlueBubbles server password/token for REST API auth
	MCPConfigEncryptionKey   string        // Key used to encrypt MCP server URLs and headers at rest
	LLMGatewayURL            string        // LLM gateway API base URL (default: "http://localhost:8080")
	LLMGatewayModel          string        // LLM gateway model name; required, startup fails if empty
	LLMGatewayEmbeddingModel string        // Optional LLM gateway embedding model used for semantic session-memory retrieval
	LLMGatewayAPIKey         string        // Optional bearer token for LLM gateway requests
	LLMGatewayVirtualKey     string        // Optional gateway routing key for LLM gateway requests
	LLMGatewayTimeout        time.Duration // Expected upstream LLM gateway timeout; local guard timeouts are derived from it
	ModelContextWindow       int           // Optional model context-window override for prompt budgeting
	ModelMaxOutputTokens     int           // Optional model output-token reserve override for prompt budgeting
	DiscordToken             string        // Discord bot token; Discord gateway disabled if empty
	SearxngURL               string        // SearXNG base URL for web search (default: "http://localhost:8888")
	MaxToolFailureRetries    int           // Maximum consecutive tool execution failures before the agent stops retrying tools (default: 3)
	WorkerPoolSize           int           // Number of concurrent broker workers (default: 1)
	LogLevel                 Level         // Logging verbosity (default: LevelInfo)
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

	return &Config{
		Port:                     getEnv("PORT", "8000"),
		WebSocketAuthSigningKey:  getEnv("WEBSOCKET_AUTH_SIGNING_KEY", ""),
		WebSocketAuthMaxTokenTTL: webSocketMaxTTL,
		IMessagePort:             getEnv("IMESSAGE_PORT", "8090"),
		IMessageWebhookPath:      getEnv("IMESSAGE_WEBHOOK_PATH", "/imessage/webhook"),
		BlueBubblesURL:           getEnv("BLUEBUBBLES_URL", ""),
		BlueBubblesPassword:      getEnv("BLUEBUBBLES_PASSWORD", ""),
		MCPConfigEncryptionKey:   getEnv("MCP_CONFIG_ENCRYPTION_KEY", ""),
		LLMGatewayURL:            getEnv("LLM_GATEWAY_URL", "http://localhost:8080"),
		LLMGatewayModel:          getEnv("LLM_GATEWAY_MODEL", ""),
		LLMGatewayEmbeddingModel: getEnv("LLM_GATEWAY_EMBEDDING_MODEL", ""),
		LLMGatewayAPIKey:         getEnv("LLM_GATEWAY_API_KEY", ""),
		LLMGatewayVirtualKey:     getEnv("LLM_GATEWAY_VIRTUAL_KEY", ""),
		LLMGatewayTimeout:        getEnvDuration("LLM_GATEWAY_TIMEOUT", 180*time.Second),
		ModelContextWindow:       getEnvInt("MODEL_CONTEXT_WINDOW", 0),
		ModelMaxOutputTokens:     getEnvInt("MODEL_MAX_OUTPUT_TOKENS", 0),
		DiscordToken:             getEnv("DISCORD_TOKEN", ""),
		SearxngURL:               getEnv("SEARXNG_URL", "http://localhost:8080"),
		MaxToolFailureRetries:    getEnvInt("MAX_TOOL_FAILURE_RETRIES", 3),
		WorkerPoolSize:           getEnvInt("WORKER_POOL_SIZE", 1),
		LogLevel:                 ParseLevel(getEnv("LOG_LEVEL", "info")),
	}, nil
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
