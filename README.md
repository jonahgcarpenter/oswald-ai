# Oswald-AI

<div align="center">

**A persistent, cross-platform digital manservant.**

Oswald is a personal AI project designed to occupy the space between a classic British butler and a hyper-advanced neural network.

![Oswald Preview](https://y.yarn.co/eb976dbd-fa42-4e1a-bb55-d3adac9785cf_text.gif) ![Oswald Preview](https://media.tenor.com/EifGeTRyvxoAAAAM/alfred-alfred-batman.gif)

</div>

Built on a "write once, serve everywhere" philosophy, Oswald acts as the connective tissue for my digital ecosystem. He seamlessly transitions between environments—managing homelab infrastructure via terminal, controlling smart home devices through IoT integrations, and facilitating social interactions within Discord communities.

Unlike stateless assistants, Oswald is architected with memory and context-awareness. He doesn't just respond to prompts; he remembers them. By building unique memory profiles for every user, he learns routines, adapts to conversational quirks, and recalls preferences across sessions. The result is a unified assistant that manages my digital life with efficiency and a touch of dry wit, ensuring the environment is exactly to my liking—often before I even ask.

## Integrations

- **Discord** There are no complex commands to remember—just mention the bot's user (`@Bot Name`) with your question or prompt.

## How It Works

<img width="1024" height="559" alt="image" src="https://github.com/user-attachments/assets/e5c227a4-7fa7-43ac-a799-994051944bba" />

## Prerequisites

Before running the application, you will need the following services available:

- **PostgreSQL w/ PGVector:** Used as a vector database, along with saving chat history for monitoring
- **SearXNG:** A running instance is required to act as the search engine tool for the bot.
- **Ollama:** Required to serve the local Large Language Model

## Installation

### Docker Compose:

```
services:
  oswald-ai:
    container_name: oswald-ai
    image: ghcr.io/jonahgcarpenter/oswald-ai/oswald-ai:latest
    environment:
      - DISCORD_TOKEN=${DISCORD_TOKEN}
      - OLLAMA_HOST_URL=${OLLAMA_HOST_URL}
      - SEARXNG_URL=${SEARXNG_URL}
      - DB_HOST=${DB_HOST}
      - DB_PORT=${DB_PORT}
      - DB_NAME=${DB_NAME}
      - DB_USER=${DB_USER}
      - DB_PASSWORD=${DB_PASSWORD}
      - DB_SCHEMA=${DB_SCHEMA}
      - LOG_LEVEL=${LOG_LEVEL}
```

### Env Example:

```
# Make a dedicated Discord App for this bot
# https://discord.com/developers/applications
DISCORD_TOKEN=your-discordbot-api-token

# Ollama
OLLAMA_HOST_URL=http://your-ollama-api-url:11434
OLLAMA_EMBEDDING_MODEL=nomic-embed-text:v1.5

# Searxng
# Web seach tool
SEARXNG_URL=http://your-searxng-url:8888

# Postgres DB
# To save each prompt and search query executed
DB_HOST=ip
DB_PORT=5432
DB_NAME=db_name
DB_USER=user
DB_PASSWORD=password
DB_SCHEMA=schema

LOG_LEVEL=DEBUG # Can be set to either INFO or DEBUG
```

## Todo

- Readd Discord integration
- Save each query/response along with any search queries used
- Config file that loads the vars
- Conditionally initialize DB tables
