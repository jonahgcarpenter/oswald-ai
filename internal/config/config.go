package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port              string
	OllamaURL         string
	OllamaRouterModel string
	WorkersConfig     string
	DiscordToken      string
}

func Load() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("Info: No .env file found, relying on system environment variables")
	}

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
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}
