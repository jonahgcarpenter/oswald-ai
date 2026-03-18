# Oswald AI - Uncensored Digital Servant

> A pure Go LLM-powered chat agent with web search integration and two-stage response generation. Fully local, fully uncensored, zero external API dependencies.

## Overview

**Oswald** is a lightweight orchestration agent that routes incoming messages through a two-stage pipeline: first, a small query model decides whether web search is needed and gathers results; second, an uncensored model generates the final response using raw search intel as context. Built in pure Go with zero external APIs—everything runs locally on your machine.

### Key Features

- **Two-Stage Pipeline**: Query generator (search decision) + Uncensored responder (final generation)
- **Web Search Integration**: SearXNG-powered search with configurable result limits (capped at 5 results to protect context window)
- **Discord Bot Support**: Full Discord integration with mention resolution, reply context, typing indicators
- **WebSocket API**: Lightweight HTTP WebSocket server
- **Streaming Responses**: Real-time response streaming to clients
- **Zero Censorship**: Fully uncensored local models—no safety filters, no corporate guardrails (besides what is searched for legal reasons)

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        External Sources                         │
├──────────────────┬──────────────────────────────────────────────┤
│   Discord User   │   WebSocket Client (testing/integration)     │
│    Messages      │          Raw Text Input                      │
└────────┬─────────┴──────────────────────────────────────────────┘
         │
    ┌────▼────────────────────────────────────────────────────────┐
    │                    GATEWAY LAYER                            │
    │  ┌──────────────────┐          ┌──────────────────────────┐ │
    │  │ DiscordGateway   │          │ WebsocketGateway         │ │
    │  ├──────────────────┤          ├──────────────────────────┤ │
    │  │ • WebSocket Conn │          │ • HTTP WS Upgrade        │ │
    │  │ • Auth + Identity│          │ • Message Streaming      │ │
    │  │ • Heartbeat Loop │          │ • JSON Response          │ │
    │  │ • Typing Status  │          │                          │ │
    │  │ • Mention Resolve│          │ Port: 8080               │ │
    │  │ • Reply Context  │          │                          │ │
    │  │ • Message Split  │          │                          │ │
    │  └────────┬─────────┘          └──────────┬───────────────┘ │
    └───────────┼──────────────────────────────┼──────────────────┘
                │                              │
                │          Prompt Text         │
                │     + Streaming Callback     │
                │                              │
    ┌───────────▼──────────────────────────────▼─────────────────┐
    │                   AGENT ORCHESTRATION                      │
    │  ┌──────────────────────────────────────────────────────┐  │
    │  │              Agent.Process()                         │  │
    │  │                                                      │  │
    │  │  1. QUERY GENERATION (with optional web search)      │  │
    │  │     ┌────────────────────────────────────┐           │  │
    │  │     │ runQueryGenerator()                │           │  │
    │  │     │ • Small model (qwen3.5:0.8b)       │           │  │
    │  │     │ • Evaluates need for web search    │           │  │
    │  │     │ • Issues web_search tool calls     │           │  │
    │  │     │ • Accumulates raw results (cap: 5) │           │  │
    │  │     │ • Returns []SearchResult           │           │  │
    │  │     │ • 60-second timeout                │           │  │
    │  │     └────────┬───────────────────────────┘           │  │
    │  │              │ Returns: []SearchResult               │  │
    │  │              │ (or empty if no search needed)        │  │
    │  │              │                                       │  │
    │  │  2. FINAL RESPONSE GENERATION                        │  │
    │  │     ┌────────────────────────────────────┐           │  │
    │  │     │ Uncensored Model Response          │           │  │
    │  │     │ • Large model (llama2-uncensored:7b)           │  │
    │  │     │ • System: Oswald personality prompt│           │  │
    │  │     │ • User: <task_briefing> with intel │           │  │
    │  │     │   (or raw prompt if no search)     │           │  │
    │  │     │ • Streaming to callback            │           │  │
    │  │     │ • 3-minute timeout                 │           │  │
    │  │     └────────┬───────────────────────────┘           │  │
    │  │              │ Returns: ChatResponse                 │  │
    │  │              │ (Content, Metrics, Thinking)          │  │
    │  │              │                                       │  │
    │  │  3. RESPONSE ASSEMBLY                                │  │
    │  │     • Extract response content                       │  │
    │  │     • Convert metrics to readable format             │  │
    │  │     • Return AgentResponse with metadata             │  │
    │  │                                                      │  │
    │  └──────────────────────────────────────────────────────┘  │
    │                                                            │
    └────────────────┬───────────────────────────────────────────┘
                     │ AgentResponse
                     │ (Model, Response, Metrics)
                     │
                ┌────▼──────────────────────────┐
                │    LLM PROVIDER LAYER         │
                │  ┌──────────────────────────┐ │
                │  │  Ollama Client (HTTP)    │ │
                │  ├──────────────────────────┤ │
                │  │ • /api/chat endpoint     │ │
                │  │ • Streaming responses    │ │
                │  │ • Tool calling support   │ │
                │  │ • Metrics collection     │ │
                │  │ • Error recovery         │ │
                │  │                          │ │
                │  │ Connected to:            │ │
                │  │ http://localhost:11434   │ │
                │  └──────────────────────────┘ │
                └───────────────────────────────┘
                     │
                     │ HTTP/REST
                     │
              ┌──────▼───────┐
              │ Ollama       │
              │ Local Models │
              │ + GPU/CPU    │
              └──────────────┘

                    SEARCH LAYER

              ┌──────────────────────┐
              │  SearXNG Client      │
              │  (HTTP)              │
              ├──────────────────────┤
              │ • Web search queries │
              │ • Result aggregation │
              │ • ~5 results per qry │
              │ • 10s timeout        │
              │                      │
              │ Connected to:        │
              │ http://localhost:8888│
              └──────────────────────┘
```

## Installation & Setup

### Prerequisites

- **Go 1.25+** (for building)
- **Ollama** running locally (default: `http://localhost:11434`)
- **SearXNG** running locally (default: `http://localhost:8888`) — optional if you don't use web search
- **Discord Token** (optional, only if using Discord gateway)

### Quick Start

#### 1. Clone the repository

```bash
git clone https://github.com/jonahgcarpenter/oswald-ai.git
cd oswald-ai
```

#### 2. Configure environment

Create a `.env` file:

| Variable         | Default                  | Purpose                                              |
| ---------------- | ------------------------ | ---------------------------------------------------- |
| `PORT`           | `8080`                   | HTTP server port (WebSocket)                         |
| `OLLAMA_URL`     | `http://localhost:11434` | Ollama API endpoint                                  |
| `SEARXNG_URL`    | `http://localhost:8888`  | SearXNG web search API endpoint                      |
| `WORKERS_CONFIG` | `config/workers.yaml`    | Query generator + response model config              |
| `DISCORD_TOKEN`  | `` (empty)               | Discord bot token (optional)                         |
| `LOG_LEVEL`      | `info`                   | Logging verbosity (`debug`, `info`, `warn`, `error`) |

##### Logging Levels

- `debug` - Full prompt inspection, iteration details, model metrics, search queries
- `info` - Operational logs (requests, responses, model selection)
- `warn` - Non-fatal issues (fallbacks, retries, timeouts)
- `error` - Critical failures

#### 3. Configure workers

Edit `config/workers.yaml` to define your models:

```yaml
workers:
  - category: "QUERY"
    model: "llama3.2:1" # Small, fast query generator
    system_prompt: |
      You are a research assistant...

  - category: "GENERAL"
    model: "llama2-uncensored:7b" # Uncensored responder
    system_prompt: |
      You are Oswald...
```

The QUERY model decides whether web search is needed and what to search for. The GENERAL model generates the final user-facing response.

#### 4. Build & Run

```bash
go run ./cmd/agent/main.go
```

### Hot Reload (Development)

Install `air` for hot-reload:

```bash
go install github.com/cosmtrek/air@latest
air
```

---

## Usage

### Discord Bot

#### In Server Channels

Mention the bot to get a response:

```
@Oswald What is the capital of France?
```

#### In DMs

Send any message directly—no mention required:

```
What is the current weather?
```

#### Replying to Oswald

You can reply to Oswald's previous message without mentioning:

```
[Reply to Oswald's message]
Can you elaborate on that?
```

Oswald will include the previous exchange as context automatically.

### WebSocket API

Connect to `ws://localhost:8080/ws`:

```bash
# Using websocat
websocat ws://localhost:8080/ws

# Send a prompt
What is Bitcoin's current price?

# Receive streaming chunks, then final JSON:
# "Bitcoin is currently..."
# {"model":"llama2-uncensored:7b","response":"..."}
```

---

## Roadmap

- [ ] Persistent conversation history (multi-user context)
- [ ] Multi-gateway response routing
- [ ] Message queue for imcoming queries
- [ ] Tool calling model for uncensored

---

## License

MIT

---

**Oswald AI** — Uncensored local intelligence for the unapologetic.
