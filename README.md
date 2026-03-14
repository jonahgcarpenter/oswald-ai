# Oswald AI - Digital Servant

> A pure Go LLM-powered chat agent with intelligent request routing and expert model selection. Zero external API dependencies—fully local, fully uncensored.

## Overview

**Oswald** is a lightweight, orchestration agent that intelligently routes incoming requests to specialized expert models based on query classification. Built in Go for minimal resource overhead, it supports multiple gateways (Discord, WebSocket) and integrates seamlessly with local Ollama installations.

### Key Features

- **Intelligent Routing**: LLM-powered triage system classifies requests into expert categories
- **Blazing Fast**: Pure Go, single-binary deployment, minimal overhead
- **Multi-Gateway Support**: Discord bot, WebSocket API
- **Zero Censorship**: Fully uncensored local models—no corporate guardrails
- **Tool-Calling Support**: Dynamic tool generation for robust routing decisions

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
    │  │ • Reply Context  │          │ Port: 8080               │ │
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
    │  │  1. TRIAGE ROUTING                                   │  │
    │  │     ┌────────────────────────────────────┐           │  │
    │  │     │ DetermineRoute()                   │           │  │
    │  │     │ • Router model classifies request  │           │  │
    │  │     │ • Calls worker tools dynamically   │           │  │
    │  │     │ • Max 3 retry attempts             │           │  │
    │  │     │ • Falls back to first worker       │           │  │
    │  │     └────────┬───────────────────────────┘           │  │
    │  │              │ Returns: RouteDecision                │  │
    │  │              │ (Category, Reason, Metrics)           │  │
    │  │              │                                       │  │
    │  │  2. EXPERT GENERATION                                │  │
    │  │     ┌────────────────────────────────────┐           │  │
    │  │     │ Provider.Chat()                    │           │  │
    │  │     │ • Expert model for selected worker │           │  │
    │  │     │ • System prompt from worker config │           │  │
    │  │     │ • Streaming to client via callback │           │  │
    │  │     │ • 3-minute timeout protection      │           │  │
    │  │     └────────┬───────────────────────────┘           │  │
    │  │              │ Returns: ChatResponse                 │  │
    │  │              │ (Content, Metrics, Thinking)          │  │
    │  │              │                                       │  │
    │  │  3. RESPONSE ASSEMBLY                                │  │
    │  │     • Adapt callbacks (string vs ChatMessage)        │  │
    │  │     • Convert metrics to readable format             │  │
    │  │     • Return AgentResponse with all metadata         │  │
    │  │                                                      │  │
    │  └──────────────────────────────────────────────────────┘  │
    │                                                            │
    └────────────────┬───────────────────────────────────────────┘
                     │ AgentResponse
                     │ (Category, Model, Content,
                     │  RouterMetrics, ExpertMetrics)
                     │
                ┌────▼──────────────────────────┐
                │    LLM PROVIDER LAYER         │
                │  ┌──────────────────────────┐ │
                │  │  Ollama Client (HTTP)    │ │
                │  ├──────────────────────────┤ │
                │  │ • /api/chat endpoint     │ │
                │  │ • Streaming responses    │ │
                │  │ • Tool calling support   │ │
                │  │ • Thinking models (o1)   │ │
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
```

## Installation & Setup

### Prerequisites

- **Go 1.25+** (for building)
- **Ollama** running locally (default: `http://localhost:11434`)
- **Discord Token** (optional, only if using Discord gateway)

### Quick Start

#### 1. Clone the repository

```bash
git clone https://github.com/jonahgcarpenter/oswald-ai.git
cd oswald-ai
```

#### 2. Configure environment

Create a `.env` file:

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `8080` | HTTP server port |
| `OLLAMA_URL` | `http://localhost:11434` | Ollama API endpoint |
| `OLLAMA_ROUTER_MODEL` | `qwen3.5:0.8b` | Router classification model |
| `WORKERS_CONFIG` | `config/workers.yaml` | Worker definitions |
| `DISCORD_TOKEN` | `` | Discord bot token (optional) |
| `LOG_LEVEL` | `info` | Logging verbosity |

##### Logging Levels

- `debug` - Verbose diagnostic info (triage attempts, metrics)
- `info` - Standard operational logging
- `warn` - Non-fatal issues (fallbacks, retries)
- `error` - Critical issues

#### 3. Set up workers

Edit `config/workers.yaml` to define your expert categories and models.

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

## API Usage

### WebSocket

Connect to `ws://localhost:8080/ws`:

```bash
# Send a prompt
websocat ws://localhost:8080/ws
hello

# Receive streaming chunks, then final JSON:
# "Hello there! How can I..."
# {...final JSON response...}
```

### Discord Bot

Simply mention the bot in any message or reply:

```
@Oswald What is the capital of France?
```

---

## Roadmap

- [ ] Persistent conversation history (multi-user context)
- [ ] Multi-gateway response routing
- [ ] Message queue for imcoming queries
- [ ] Tool calling for expert models

---

## License

MIT

---

**Oswald AI** - Built for those (me) who want uncensored, locally-hosted intelligence... sort of.
