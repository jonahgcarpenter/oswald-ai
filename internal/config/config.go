package config

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                  string
	OllamaURL             string
	OllamaRouterModel     string
	OllamaComplexModel    string
	OllamaCodingModel     string
	OllamaUncensoredModel string
	OllamaSimpleModel     string
}

func Load() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("Info: No .env file found, relying on system environment variables")
	}

	return &Config{
		// Main
		Port: getEnv("PORT", "8080"),

		// Ollama
		OllamaURL:             getEnv("OLLAMA_URL", "http://localhost:11434"),
		OllamaRouterModel:     getEnv("OLLAMA_ROUTER_MODEL", "llama3.2:3b"),
		OllamaComplexModel:    getEnv("OLLAMA_COMPLEX_MODEL", "llama3.1:8b"),
		OllamaCodingModel:     getEnv("OLLAMA_CODING_MODEL", "qwen2.5-coder:7b"),
		OllamaUncensoredModel: getEnv("OLLAMA_UNCENSORED_MODEL", "llama2-uncensored:7b"),
		OllamaSimpleModel:     getEnv("OLLAMA_SIMPLE_MODEL", "llama3.2:3b"),
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if value, exists := os.LookupEnv(key); exists {
		boolValue, err := strconv.ParseBool(value)
		if err == nil {
			return boolValue
		}
	}
	return fallback
}
