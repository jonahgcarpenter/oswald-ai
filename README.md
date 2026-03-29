# Oswald AI - Uncensored Digital Servant

> Fully local, fully uncensored, zero costly API dependencies.

## Overview

Oswald AI is a local-first Go application built around a single agentic chat loop.
The model receives the user prompt, can call registered tools such as `web_search`, and then returns a final streamed response in Oswald's voice.

## Features

- Single-pass agentic loop with tool execution and streaming
- SearXNG-backed `web_search` tool
- Discord bot gateway with mention resolution, reply context, typing indicators, and message splitting
- Local WebSocket gateway at `ws://localhost:8080/ws`
- Fully local Ollama integration with streaming and metrics

---

## Prerequisites

- Go 1.25+
- Ollama running locally, default `http://localhost:11434`
- SearXNG running locally, default `http://localhost:8888`, if you want web search
- Optional Discord bot token for the Discord gateway

---

## Configuration

### Worker Configuration:

`config/workers.yaml` currently defines a single required worker:

- `GENERAL` - the only runtime worker used by `Agent.Process()`

The model name and system prompt both live there, so behavior changes do not require code changes.

### Environment variables:

| Variable         | Default                  | Purpose                              |
| ---------------- | ------------------------ | ------------------------------------ |
| `PORT`           | `8080`                   | Local WebSocket server port          |
| `OLLAMA_URL`     | `http://localhost:11434` | Ollama base URL                      |
| `SEARXNG_URL`    | `http://localhost:8888`  | SearXNG base URL                     |
| `WORKERS_CONFIG` | `config/workers.yaml`    | Worker config path                   |
| `TOOLS_CONFIG`   | `config/tools`           | Tool definition directory            |
| `DISCORD_TOKEN`  | empty                    | Enables Discord gateway when set     |
| `MAX_ITERATIONS` | `5`                      | Max tool-call iterations per request |
| `LOG_LEVEL`      | `info`                   | `debug`, `info`, `warn`, `error`     |

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
What is Bitcoins current price?

# Receive streaming chunks, then final JSON:
# "Bitcoin is currently..."
# {"model":"llama2-uncensored:7b","response":"..."}
```

---

## Roadmap

- [x] Nncensored Tool calling model
- [ ] Multi-gateway response routing and queuing
- [ ] Persistent conversation history (multi-user context)
- [ ] Support for images, files, and URLs to search in a prompt
- [ ] STT & TTS support

---

## License

MIT

---

**Oswald AI** — Uncensored local unintelligence for the unapologetic.
