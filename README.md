# Oswald AI - Uncensored Digital Servant

> Fully local, fully uncensored, zero costly API dependencies.

## Overview

Oswald AI is a local-first Go application built around a single agentic chat loop.
The model receives the user prompt, can call registered tools, and then returns a final streamed response in Oswald's voice.

## Features

- Iterative tool-calling agent loop on top of an OpenAI-compatible LLM gateway
- iMessage, Discord, and WebSocket gateway
- HACS integration for Home Assistant [here](https://github.com/jonahgcarpenter/has-oswald-conversation)
- Builtin `web.search`, `time.current`, `memory.save`, `memory.search`, `memory.list`, `memory.forget`, `soul.read`, and `soul.patch` tools
- User & Global MCP integrations
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

Designed for the Home Assistant integration [here](https://github.com/jonahgcarpenter/has-oswald-conversation). The integration must be updated to issue or receive signed subject tokens before connecting to an authenticated Oswald deployment.

WebSocket connections require a short-lived signed token. Generate a signing key once and configure it in Oswald:

```bash
export WEBSOCKET_AUTH_SIGNING_KEY="$(openssl rand -base64 32)"
export WEBSOCKET_AUTH_MAX_TOKEN_TTL=15m
```

Issue a token whose subject is the stable WebSocket user identity, then connect with a bearer header:

```bash
TOKEN="$(go run ./cmd/ws-token -subject alice -name Alice -ttl 15m)"
websocat -H="Authorization: Bearer $TOKEN" ws://127.0.0.1:8000/ws

# Send a prompt
What is Bitcoins current price?

# Receive typed streaming chunks, then final JSON:
# {"type":"content","text":"Bitcoin is currently..."}
# {"model":"<gateway-route-or-model>","response":"..."}
```

Structured clients may continue sending `user_id`, but it must match the signed token subject and cannot select request ownership. Plain-text payloads use the same authenticated subject. A connection is permanently bound to one subject.

Use `wss://` through a TLS-terminating reverse proxy whenever traffic leaves a trusted host. Signing authenticates identity but does not encrypt `ws://` traffic. The Home Assistant integration must provide an `Authorization: Bearer <token>` header and mint or receive a subject-bound token for each connection.

The native browser `WebSocket` API cannot set an `Authorization` header. Browser clients require a trusted proxy or a future cookie/session authentication mode; the signed-token transport is intended for service and command-line clients.

Treat `WEBSOCKET_AUTH_SIGNING_KEY` as issuer authority: anyone holding it can mint a token for any WebSocket subject. Give it only to Oswald and trusted identity-issuing services, never end users or browser clients.

## Commands

Commands are gateway-level slash commands. They are handled before requests reach the model.

### User Commands

| Command | Usage | Description |
| --- | --- | --- |
| `/help` | `/help [command]` | List available commands or show usage for one command. |
| `/connect` | `/connect [code\|cancel]` | In a direct chat, create a 10-minute one-time code or confirm a code from another authenticated account. |
| `/disconnect` | `/disconnect [account_number]` | In a direct chat, disconnect a linked gateway account. The last linked account cannot be removed. |
| `/reset` | `/reset` | Clear this conversation's session history and load the latest stable tenant profile. |
| `/mcp servers` | `/mcp servers` | List your user-scoped MCP servers. |
| `/mcp add` | `/mcp add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>]` | Add or update a user-scoped MCP server. URLs and headers are encrypted at rest. |
| `/mcp remove` | `/mcp remove <name>` | Remove one of your MCP servers. |
| `/mcp enable` | `/mcp enable <name>` | Enable one of your MCP servers. |
| `/mcp disable` | `/mcp disable <name>` | Disable one of your MCP servers. |
| `/mcp test` | `/mcp test <name>` | Connect to one of your MCP servers and report discovered tool count. |

Eligible durable identity and preference memories are compiled into a bounded, lower-authority tenant profile. A profile is frozen for each conversation session; changes become automatic context in new sessions or after `/reset`, while deeper memory remains available through memory tools.

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
