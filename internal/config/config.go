package config

import (
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	Port            string        // HTTP port for the WebSocket gateway (default: "8080")
	OllamaURL       string        // Ollama API base URL (default: "http://localhost:11434")
	OllamaModel     string        // Ollama model name; required — startup fails if empty
	DiscordToken    string        // Discord bot token; Discord gateway disabled if empty
	SearxngURL      string        // SearXNG base URL for web search (default: "http://localhost:8888")
	MaxIterations   int           // Maximum tool-call iterations in the agentic loop (default: 5)
	WorkerPoolSize  int           // Number of concurrent broker workers (default: 1)
	LogLevel        Level         // Logging verbosity (default: LevelInfo)
	MemoryMaxTurns  int           // Maximum retained conversation turn pairs per session; 0 disables the limit
	MemoryMaxAge    time.Duration // Maximum age for retained conversation turn pairs; 0 disables expiry
	PromptDebugPath string        // Optional directory path; when set, each request's full prompt is written to a Markdown file for inspection
}

const (
	DefaultSoulPath       = "config/soul.md"
	DefaultToolsConfigDir = "config/tools"
	DefaultUserMemoryPath = "data/memory/users"
)

// Load reads configuration from environment variables, with .env file support.
// Missing variables fall back to sensible defaults.
func Load() *Config {
	// Silently ignore missing .env — production environments use real env vars
	godotenv.Load() // nolint: errcheck

	return &Config{
		Port:            getEnv("PORT", "8080"),
		OllamaURL:       getEnv("OLLAMA_URL", "http://localhost:11434"),
		OllamaModel:     getEnv("OLLAMA_MODEL", "jaahas/qwen3.5-uncensored:4b"),
		DiscordToken:    getEnv("DISCORD_TOKEN", ""),
		SearxngURL:      getEnv("SEARXNG_URL", "http://localhost:8888"),
		MaxIterations:   getEnvInt("MAX_ITERATIONS", 5),
		WorkerPoolSize:  getEnvInt("WORKER_POOL_SIZE", 1),
		LogLevel:        ParseLevel(getEnv("LOG_LEVEL", "info")),
		MemoryMaxTurns:  getEnvInt("MEMORY_MAX_TURNS", 10),
		MemoryMaxAge:    getEnvDuration("MEMORY_MAX_AGE", 0),
		PromptDebugPath: getEnv("PROMPT_DEBUG_PATH", ""),
	}
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
