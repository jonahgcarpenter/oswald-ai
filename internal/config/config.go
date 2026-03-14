package config

import (
	"os"

	"github.com/joho/godotenv"
)

// Config holds all environment-based application settings loaded at startup.
// All fields are required unless a fallback default is provided in Load().
type Config struct {
	Port              string // HTTP server port (default: "8080")
	OllamaURL         string // Base URL for Ollama API (default: "http://localhost:11434")
	OllamaRouterModel string // Router model name for triage (default: "qwen3.5:0.8b")
	WorkersConfig     string // Path to workers.yaml (default: "config/workers.yaml")
	DiscordToken      string // Discord bot token (required for Discord gateway)
	LogLevel          Level  // Logging verbosity (default: info)
}

// Load constructs a Config from environment variables with sensible defaults.
// Attempts to load a .env file first (failures are silent; this is normal in production).
// All fields default to safe development values if the env var is unset.
func Load() *Config {
	// Attempt to load .env before anything else so LOG_LEVEL can come from it.
	// Failures are expected in production; they're logged after the logger is set up.
	godotenv.Load() // nolint: errcheck

	return &Config{
		Port: getEnv("PORT", "8080"),

		OllamaURL:         getEnv("OLLAMA_URL", "http://localhost:11434"),
		OllamaRouterModel: getEnv("OLLAMA_ROUTER_MODEL", "qwen3.5:0.8b"),

		WorkersConfig: getEnv("WORKERS_CONFIG", "config/workers.yaml"),

		DiscordToken: getEnv("DISCORD_TOKEN", ""),

		LogLevel: ParseLevel(getEnv("LOG_LEVEL", "info")),
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
