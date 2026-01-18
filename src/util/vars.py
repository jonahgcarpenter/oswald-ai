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

    # NOTE: General
    API_VERSION: str = "v2"

    # NOTE: MCP
    DISCORD_TOKEN: str

    # NOTE: Ollama
    OLLAMA_URL: str
    OLLAMA_AGENT_MODEL: str


try:
    settings = Settings()

except Exception as e:
    print(f"FATAL: Failed to load application settings: {e}", file=sys.stderr)
    sys.exit("Failed to load configuration. Exiting.")
