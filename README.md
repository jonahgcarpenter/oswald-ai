# Oswald AI - Uncensored Digital Servant

> Fully local, fully uncensored, zero costly API dependencies.

## Overview

Oswald AI is a local-first Go application built around a single agentic chat loop.
The model receives the user prompt, can call registered tools, and then returns a final streamed response in Oswald's voice.

## Features

- Iterative tool-calling agent loop on top of an OpenAI-compatible LLM gateway
- iMessage, Discord, and WebSocket gateway
- Builtin `web.search`, `memory.save`, `memory.search`, `memory.list`, `memory.forget`, `soul.read`, `soul.patch`, and `session.summary` tools
- MCP integration starting with Github
- In-process chat memory with TTL, max-turn retention, and prompt-budget compaction
- Per-user persistent memory on disk and a live-editable soul file
- Fully local runtime with no hosted model dependency

## Memory

Oswald uses three memory layers:

- Soul memory: `data/memory/soul/soul.md`, read fresh on every request
- Persistent user memory: `data/memory/users/*.md`, managed by `memory.*` tools
- Session chat memory: in-process only, pruned by TTL and max-turn limits, then compacted again if prompt budget is exceeded

`AGENTS.md` documents the full runtime and architecture in detail.

## Usage

### Discord/iMessage Bot

In DMs or direct chats, send any message:

```text
What is the current weather?
```

In server channels or group chats, mention Oswald:

```text
@Oswald What is the capital of France?
```

You can also reply to a message and mention Oswald to include that message as context:

```text
[Replying to Jonah: "The capital of the US is New York"]
@Oswald Is this true?
```

Replies to Oswald do not need another mention:

```text
[Reply to Oswald's message]
Can you elaborate on that?
```

### WebSocket API

Currently used for testing.

Connect to `ws://localhost:8000/ws`:

```bash
# Using websocat
websocat ws://localhost:8000/ws

# Send a prompt
What is Bitcoins current price?

# Receive streaming chunks, then final JSON:
# "Bitcoin is currently..."
# {"model":"<gateway-route-or-model>","response":"..."}
```

## Roadmap

- [x] Uncensored tool calling model
- [x] Multi-gateway response routing and queuing
- [x] Persistent conversation history (multi-user context)
- [x] Support for images
- [ ] Support for files
- [ ] STT & TTS support
