# Oswald AI - Uncensored Digital Servant

> Fully local, fully uncensored, zero costly API dependencies.

## Overview

Oswald AI is a local-first Go application built around a single agentic chat loop.
The model receives the user prompt, can call registered tools, and then returns a final streamed response in Oswald's voice.

## Features

- Iterative tool-calling agent loop on top of an OpenAI-compatible LLM gateway
- iMessage, Discord, and WebSocket gateway
- Builtin `web.search`, `memory.save`, `memory.search`, `memory.list`, `memory.forget`, `soul.read`, and `soul.patch` tools
- User MCP integrations
- SQLite-backed session chat memory with TTL expiry
- Per-user persistent memory in SQLite and a live-editable soul file
- Fully local runtime with no cloud hosted model dependency

## Memory

Oswald uses three memory layers:

- Soul memory: `data/memory/soul/soul.md`, read fresh on every request
- Persistent user memory: `data/database/oswald.db`, managed by `memory.*` tools
- Session chat memory: recent completed exchanges in SQLite `session_turns`, expired by TTL

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

## Commands

Commands are gateway-level slash commands. They are handled before requests reach the model.

### User Commands

| Command | Usage | Description |
| --- | --- | --- |
| `/help` | `/help [command]` | List available commands or show usage for one command. |
| `/connect` | `/connect [gateway_number identifier]` | Link another gateway account to your canonical user. Run without arguments to list gateway options. |
| `/disconnect` | `/disconnect [account_number]` | Disconnect a linked gateway account. Run without arguments to list connected accounts. The last linked account cannot be removed. |
| `/mcp servers` | `/mcp servers` | List your user-scoped MCP servers. |
| `/mcp add` | `/mcp add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>]` | Add or update a user-scoped MCP server. URLs and headers are encrypted at rest. |
| `/mcp remove` | `/mcp remove <name>` | Remove one of your MCP servers. |
| `/mcp enable` | `/mcp enable <name>` | Enable one of your MCP servers. |
| `/mcp disable` | `/mcp disable <name>` | Disable one of your MCP servers. |
| `/mcp test` | `/mcp test <name>` | Connect to one of your MCP servers and report discovered tool count. |

### Admin Commands

| Command | Usage | Description |
| --- | --- | --- |
| `/users` | `/users` | List canonical users. |
| `/user` | `/user <canonical_id>` | Show one canonical user's account, admin, and ban details. |
| `/admin` | `/admin <canonical_id>` | Grant admin access to a user. |
| `/unadmin` | `/unadmin <canonical_id>` | Remove admin access from a user. |
| `/ban` | `/ban <canonical_id> [reason]` | Ban a user from using Oswald. |
| `/unban` | `/unban <canonical_id>` | Unban a user. |
| `/mcp global servers` | `/mcp global servers` | List global MCP servers visible to all users. |
| `/mcp global add` | `/mcp global add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>]` | Add or update a global MCP server. URLs and headers are encrypted at rest. |
| `/mcp global remove` | `/mcp global remove <name>` | Remove a global MCP server. |
| `/mcp global enable` | `/mcp global enable <name>` | Enable a global MCP server. |
| `/mcp global disable` | `/mcp global disable <name>` | Disable a global MCP server. |
| `/mcp global test` | `/mcp global test <name>` | Connect to a global MCP server and report discovered tool count. |

## Roadmap

- [x] Uncensored tool calling model
- [x] Multi-gateway response routing and queuing
- [x] Persistent conversation history (multi-user context)
- [ ] Support for images, gifs, and files
  - [x] Images
  - [x] GIFs
  - [ ] Files
- [x] Global vs User defined MCP servers
- [ ] Scheduled task (cron tool)
- [ ] STT & TTS support
