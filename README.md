# Oswald AI - Uncensored Digital Servant

> Fully local, fully uncensored, zero costly API dependencies.

## Overview

Oswald AI is a local-first Go application built around a single agentic chat loop.
The model receives the user prompt, can call registered tools, and then returns a final streamed response in Oswald's voice.

## Features

- Iterative tool-calling agent loop on top of Ollama
- iMessage, Discord, and WebSocket gateway
- Builtin `web_search`, `persistent_memory`, and `soul_memory` tools
- In-process chat memory with TTL, max-turn retention, and prompt-budget compaction
- Per-user persistent memory on disk and a live-editable soul file
- Fully local runtime with no hosted model dependency

## Prerequisites

- Go 1.25+
- Ollama running locally, default `http://localhost:11434`
- SearXNG running locally, default `http://localhost:8888`, if you want web search
- Optional Discord bot token for the Discord gateway
- Optional BlueBubbles server for the iMessage gateway

## Configuration

### Environment variables:

| Variable                   | Default                        | Purpose                                         |
| -------------------------- | ------------------------------ | ----------------------------------------------- |
| `OLLAMA_MODEL`             | `jaahas/qwen3.5-uncensored:4b` | Ollama model name                               |
| `OLLAMA_URL`               | `http://localhost:11434`       | Ollama API base URL                             |
| `PORT`                     | `8080`                         | WebSocket gateway port                          |
| `DISCORD_TOKEN`            | empty                          | Enables Discord gateway when set                |
| `BLUE_BUBBLES_URL`         | empty                          | Enables iMessage gateway when set               |
| `BLUE_BUBBLES_PASSWORD`    | empty                          | Enables iMessage gateway when set               |
| `IMESSAGE_PORT`            | `8090`                         | The port listening for BlueBubbles webhooks     |
| `IMESSAGE_WEBHOOK_PATH`    | `/imessage/webhook`            | The endpoint listening for BlueBubbles webhooks |
| `SEARXNG_URL`              | `http://localhost:8888`        | SearXNG base URL                                |
| `WORKER_POOL_SIZE`         | `1`                            | Broker worker count                             |
| `MAX_TOOL_FAILURE_RETRIES` | `3`                            | Consecutive tool failure limit per request      |
| `MEMORY_MAX_TURNS`         | `10`                           | Max retained session turn pairs; `0` disables   |
| `MEMORY_MAX_AGE`           | `30m`                          | Max retained session age; `0` disables          |
| `PROMPT_DEBUG_PATH`        | empty                          | Writes per-request prompt debug markdown dumps  |
| `LOG_LEVEL`                | `info`                         | Log verbosity                                   |

## Memory

Oswald uses three memory layers:

- Soul memory: `config/soul.md`, read fresh on every request
- Persistent user memory: `config/memory/users/*.md`, managed by `persistent_memory`
- Session chat memory: in-process only, pruned by TTL and max-turn limits, then compacted again if prompt budget is exceeded

`AGENTS.md` documents the full runtime and architecture in detail.

## Usage

### Discord/iMessage Bot

#### In Server Channels or Group Chats

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

## Roadmap

- [x] Uncensored tool calling model
- [x] Multi-gateway response routing and queuing
- [x] Persistent conversation history (multi-user context)
- [x] Support for images
- [ ] Support for files
- [ ] STT & TTS support

## License

MIT
