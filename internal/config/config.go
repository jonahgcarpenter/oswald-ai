package config

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port              string
	OllamaURL         string
	OllamaRouterModel string
	WorkersConfig     string
	DiscordToken      string
	LogLevel          Level
}

func Load() *Config {
	// Attempt to load .env before anything else so LOG_LEVEL can come from it.
	// Failures are expected in production and logged after the logger is set up.
	godotenv.Load() // nolint: errcheck

	return &Config{
		// Main
		Port: getEnv("PORT", "8080"),

		// Ollama
		OllamaURL:         getEnv("OLLAMA_URL", "http://localhost:11434"),
		OllamaRouterModel: getEnv("OLLAMA_ROUTER_MODEL", "qwen3.5:0.8b"),

		// Worker agent registry
		WorkersConfig: getEnv("WORKERS_CONFIG", "config/workers.yaml"),

		// Discord
		DiscordToken: getEnv("DISCORD_TOKEN", ""),

		// Logging
		LogLevel: ParseLevel(getEnv("LOG_LEVEL", "info")),
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}
