# Oswald-AI

Oswald is a high-performance, uncensored, and fully self-hosted AI agent designed to act as the central nervous system for a modern smart home. Built on the principle of total local sovereignty, Oswald operates entirely within your private infrastructure—no telemetry, no external filters, and no corporate guardrails.

## Integrations

- **Discord** There are no complex commands to remember—just mention the bot's user (`@Bot Name`) with your question or prompt.

## How It Works

<img width="8191" height="3711" alt="architecture diagram" src="https://github.com/user-attachments/assets/cd6d9569-2d7a-4d74-bedf-3ef4438a3d11" />

## Prerequisites

Before running the application, you will need the following services available:

- **PostgreSQL w/ PGVector:** Used as a vector database, along with saving chat history for monitoring
- **SearXNG:** A running instance is required to act as the search engine tool for the bot.
- **Ollama:** Required to serve the local Large Language Model

## Installation

All default variables can be found [here](https://github.com/jonahgcarpenter/oswald-ai/blob/master/app/utils/config.py)

### Docker Compose:

```
services:
  oswald-ai:
    container_name: oswald-ai
    image: ghcr.io/jonahgcarpenter/oswald-ai/oswald-ai:latest
    environment:
      - DISCORD_TOKEN=${DISCORD_TOKEN}
      - OLLAMA_BASE_URL=${OLLAMA_BASE_URL}
      - OLLAMA_BASE_MODEL=${OLLAMA_BASE_MODEL}
      - OLLAMA_EMBEDDING_MODEL=${OLLAMA_EMBEDDING_MODEL}
      - SEARXNG_URL=${SEARXNG_URL}
      - DATABASE_URL=${DATABASE_URL}
      - DATABASE_SCHEMA=${DATABASE_SCHEMA}
      - LOG_LEVEL=${LOG_LEVEL}
```
