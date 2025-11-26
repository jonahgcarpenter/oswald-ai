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

All default variables can be found [here](https://github.com/jonahgcarpenter/oswald-ai/blob/refactor/app/utils/config.py)

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

## Todo

- Readd Discord integration
- Conditionally initialize DB tables
