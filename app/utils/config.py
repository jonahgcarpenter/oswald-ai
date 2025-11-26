import sys

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """
    Defines the application's configuration settings.

    Pydantic will automatically read from the environment or a .env file.
    """

    model_config = SettingsConfigDict(
        env_file=".env", env_file_encoding="utf-8", extra="ignore"
    )

    DISCORD_TOKEN: str = ""

    OLLAMA_BASE_URL: str = "http://localhost:11434"

    OLLAMA_BASE_MODEL: str = "artifish/llama3.2-uncensored:latest"

    OLLAMA_EMBEDDING_MODEL: str = "nomic-embed-text:v1.5"

    SEARXNG_URL: str = "http://localhost:8888"

    DATABASE_URL: str = "postgresql://oswald_ai:password@localhost:5432/oswald_ai"

    DATABASE_SCHEMA: str = "oswald_ai"

    LOG_LEVEL: str = "INFO"


try:
    settings = Settings()

except Exception as e:
    print(f"FATAL: Failed to load application settings: {e}", file=sys.stderr)
    sys.exit("Failed to load configuration. Exiting.")
