# AGENTS.md — Oswald AI Developer Reference

This file is the internal reference for how Oswald AI works today.

## Project Overview

Oswald AI is a pure Go application built around a single Ollama-backed agent loop.
It exposes that loop through Discord, a local WebSocket gateway, and an iMessage gateway backed by BlueBubbles, and supports three builtin tools:

- `web_search`
- `persistent_memory`
- `soul_memory`

Oswald now supports multimodal user input for the active turn: text-only, image-only, and text-plus-image requests can be sent through every gateway when the active Ollama model supports images.

There is no JavaScript, TypeScript, or frontend code in this repository.

## Runtime Architecture

Current layers:

1. `cmd/agent/main.go` — startup wiring
2. `internal/gateway/` — gateway bootstrap and implementations
3. `internal/broker/` — request queue and worker pool
4. `internal/agent/` — iterative tool-calling agent loop
5. `internal/memory/` — in-process conversation retention and compaction
6. `internal/tools/` — tool registry, schemas, and builtin handlers
7. `internal/ollama/` — Ollama client and request/response schema

## Startup Flow

`cmd/agent/main.go` performs startup in this order:

1. Load environment config
2. Create the shared logger
3. Create the Ollama client
4. Discover context budget from Ollama `/api/show`
5. Create the soul store and persistent user-memory store
6. Load tool schemas from `config/tools/*.md` and register builtin handlers
7. Build enabled gateways from config
8. Create the in-process conversation memory store
9. Create the agent
10. Start the broker worker pool
11. Start each gateway in its own goroutine
12. Wait for shutdown signal and drain the broker

## Request Lifecycle

Every request follows the same high-level path:

1. A gateway receives user input
2. The gateway derives routing metadata such as `SessionKey`, `SenderID`, and `DisplayName`, and may attach current-turn images
3. The gateway submits a `broker.Request`
4. A broker worker calls `(*Agent).Process()`
5. The agent builds the prompt, includes any current-turn images on the final user message, runs Ollama, executes tool calls if requested, and loops until the model stops calling tools
6. The final response is returned to the originating gateway
7. The gateway sends the response back to the client, Discord channel, or iMessage chat

The loop is iterative, not single-pass. The model may call tools zero or more times before producing a final answer.

## Broker

The broker lives in `internal/broker/` and sits between gateways and the agent.

- Requests are queued through a shared buffered channel
- A fixed worker pool limits concurrent `Process()` calls
- If the queue is full, the broker returns an immediate fallback response instead of blocking forever
- Shutdown closes the queue and waits for in-flight work to finish

Relevant config:

- `WORKER_POOL_SIZE` default: `1`

## Agent Flow

The core runtime is `(*Agent).Process()` in `internal/agent/agent.go`.

Per request it does the following:

1. Create a request-scoped timeout of `3*time.Minute`
2. Inject `SenderID` into context so tools can identify the current user
3. Read `config/soul.md` fresh from disk
4. Build the dynamic system prompt from:
   - soul content
   - current date and time
   - current speaker identity when available
5. Load retained session turns from `internal/memory`
6. Compact older turns if the estimated prompt exceeds the active model budget
7. Build the Ollama message array: system prompt, retained history, current user prompt, and any current-turn images
8. Optionally write a prompt debug dump when `PROMPT_DEBUG_PATH` is set
9. Call Ollama with all registered tools available
10. If the model emits tool calls:
    - execute each tool handler
    - append tool results as `tool` messages
    - repeat until no tool calls remain or the consecutive tool-failure limit is hit
11. If tool failures exhaust the retry budget, make one final model call with tools disabled
12. Persist only the final user message and final assistant reply to session memory
13. Return the final `AgentResponse`

Multimodal request notes:

- Images are attached only to the current user turn; they are not replayed into future turns
- Session memory stays text-only; image-bearing turns are stored with a short attachment marker instead of raw image data
- Prompt debug dumps include image counts and metadata, not base64 payloads

Streaming behavior:

- WebSocket clients can receive `thinking`, `content`, and `status` chunks while the request is running
- Discord does not stream token-by-token; it waits for the final response
- iMessage does not stream token-by-token; it waits for the final response

## Three-Layer Memory Model

Oswald keeps three distinct memory layers.

| Layer | Storage | Purpose | Mutable by agent |
| --- | --- | --- | --- |
| Soul memory | `config/soul.md` | Identity, directives, personality | Yes |
| Persistent user memory | `config/memory/users/<id>.md` | Facts about a user that survive restart | Yes |
| Session chat memory | In-process only | Conversation history for the active session | Implicitly |

### Soul Memory

- Stored in `config/soul.md`
- Read fresh on every request
- Edited through the `soul_memory` tool
- Changes take effect on the next request without restart

### Persistent User Memory

- Stored in `config/memory/users/<id>.md`
- Managed by the `persistent_memory` tool
- Organized into categories: `identity`, `preferences`, `context`, `notes`
- Uses per-user locking so different users can be updated in parallel safely
- Older flat files are migrated to categorized markdown on first recall or write
- `<id>` is now Oswald's canonical internal user ID, not a raw gateway account ID

### Account Links

- Stored in `config/accounts/links.json`
- Maps external gateway accounts like Discord, WebSocket, and iMessage to canonical internal user IDs
- Lets persistent memory stay shared across gateways while session chat memory remains gateway/thread scoped
- `/connect` and `/disconnect` operate on this store before any request reaches the agent loop

### Session Chat Memory

- Stored only in memory until process exit
- Keyed by a gateway-provided `SessionKey`
- Stores only final user/assistant turn pairs
- Tool messages and intermediate reasoning are intentionally not persisted

Retention behavior:

- `MEMORY_MAX_TURNS` keeps only the newest N turn pairs when set above `0`
- `MEMORY_MAX_AGE` expires turn pairs older than the configured Go duration when set above `0`
- Pruning is destructive inside the store

Prompt-budget behavior:

- The agent estimates prompt size before calling Ollama
- If retained history would exceed budget, the oldest turns are summarized into a synthetic replacement turn
- That compacted turn is written back into session memory and gets a fresh timestamp
- Compaction is destructive and still counts toward `MEMORY_MAX_TURNS`

## Context Budget Discovery

Context budgeting lives in `internal/memory/budget.go`.

- At startup the app queries Ollama `/api/show`
- `num_ctx` from model parameters is preferred
- `*.context_length` from model metadata is the fallback
- If discovery fails, package defaults are used

The prompt budget is the context window minus reserves for:

- response generation
- tool overhead
- safety margin

## Gateways

Gateway bootstrap is in `internal/gateway/bootstrap.go`.

- WebSocket is always enabled
- Discord is enabled only when `DISCORD_TOKEN` is set
- iMessage is enabled only when both `BLUEBUBBLES_URL` and `BLUEBUBBLES_PASSWORD` are set

### WebSocket Gateway

Files:

- `internal/gateway/websocket/gateway.go`
- `internal/gateway/websocket/types.go`

Behavior:

- Listens on `/ws`
- Accepts either plain text or JSON input
- JSON input fields:
  - `user_id`
  - `display_name`
  - `prompt`
  - `images`
- If plain text is sent, the remote address is used as fallback identity
- If `user_id` is present, it becomes the primary session key
- Supports text-only, image-only, and text-plus-image JSON requests
- Streams typed chunks during generation, then sends a final JSON response payload

WebSocket image payloads use the shape:

- `mime_type`
- `data` (base64-encoded image bytes)
- `source` (optional filename/label)

### Discord Gateway

Files:

- `internal/gateway/discord/gateway.go`
- `internal/gateway/discord/types.go`

Behavior:

- Maintains a reconnecting Discord Gateway websocket session
- Sends heartbeats and identifies with the configured bot token
- Ignores bot-authored messages
- In guilds, only responds to mentions or direct replies to the bot
- In DMs, responds to any message
- Resolves Discord mentions into readable `@username` text
- Downloads supported image attachments from incoming messages and includes them on the current user turn
- Sends typing indicators while the request is running
- Splits long replies to stay under Discord's 2000-character limit
- Supports text-only, image-only, and text-plus-image messages

Discord session keys use a hybrid strategy:

- DM: `SenderID`
- Guild channel or thread: `ChannelID:SenderID`

This prevents cross-talk between users in the same Discord channel while preserving continuity in DMs.

Reply handling:

- Replies to non-bot messages inject quoted context into the prompt
- Replies to Oswald messages try to reuse the same session when possible
- A short-lived reply index tracks which session a prior Oswald message came from

### iMessage Gateway

Files:

- `internal/gateway/imessage/gateway.go`
- `internal/gateway/imessage/types.go`

Behavior:

- Listens for BlueBubbles webhook events on a dedicated HTTP port and path
- Ignores self-authored messages and payloads with neither text nor attachments
- Normalizes iMessage handles into canonical phone-number or email identifiers
- Resolves account links using contact display names when available, with identifier fallback
- In direct chats, responds to all messages; in group chats, responds only to `@oswald`, account-link commands, or replies to Oswald
- Downloads supported image attachments from BlueBubbles by attachment GUID and includes them on the current user turn
- Sends typing indicators and replies back through the BlueBubbles REST API
- Tracks a short-lived in-memory message index so reply context can be reused across follow-up messages
- Supports text-only, image-only, and text-plus-image messages

iMessage session keys use a hybrid strategy:

- DM: `imessage:dm:<normalized-sender-id>`
- Group chat: `imessage:<chat-guid>:<normalized-sender-id>`

This preserves per-user continuity in direct chats while avoiding cross-talk inside group conversations.

Reply handling:

- Replies to non-bot messages inject quoted context into the prompt
- Replies to Oswald messages reuse session memory when the reply stays in the same session
- Cross-session replies to prior Oswald messages inject quoted fallback context when needed

## Tools

Tools are split into schema and runtime layers.

- Schemas are loaded from `config/tools/*.md`
- Runtime handlers are wired in `internal/tools/bootstrap.go`

Current builtin tools:

- `web_search` — SearXNG-backed search
- `persistent_memory` — remember, recall, and forget user facts
- `soul_memory` — read, write, or append to the soul file

### Tool Registry

The registry:

- loads markdown specs from disk
- converts them into Ollama tool schemas
- maps tool names to handlers
- executes handlers when the model issues tool calls

### Tool Failure Handling

- Tool execution errors are converted into tool-response messages so the model can recover
- Consecutive failures are tracked per request
- Once `MAX_TOOL_FAILURE_RETRIES` is reached, the agent stops offering tools for that request and asks the model to finish without them

## Ollama Integration

Files:

- `internal/ollama/client.go`
- `internal/ollama/schema.go`
- `internal/ollama/types.go`

Notes:

- Ollama is the only model backend
- `/api/show` is used at startup for context-budget discovery
- `/api/chat` is used for normal requests, tool calling, and streaming
- The client maps between internal app types and Ollama's wire format
- Streaming responses accumulate both `thinking` and visible content
- Current-turn images are sent to Ollama on the user message `images` field when provided by a gateway

## Image Validation

Image validation is centralized in `internal/media/images.go`.

- Supported MIME types:
  - `image/jpeg`
  - `image/png`
  - `image/webp`
- Maximum images per request: `4`
- Maximum size per image: `10 MiB`
- WebSocket validates the declared MIME type and base64 payload supplied by the client
- Discord and iMessage validate attachment metadata, enforce size limits, then validate the downloaded bytes using HTTP `Content-Type` and content sniffing
- BlueBubbles commonly converts HEIC camera images to JPEG before Oswald receives them, so explicit HEIC support is not currently required

## Prompt Debug Dumps

Set `PROMPT_DEBUG_PATH` to enable per-request markdown debug dumps.

Each dump includes:

- model and session metadata
- estimated token counts before and after pruning
- number of compacted turn pairs
- the exact message array sent to Ollama
- the tool schemas included in the request

Implementation: `internal/debug/prompt.go`

## Build, Run, and Verification

```bash
go run ./cmd/agent/main.go
go build -o ./tmp/main ./cmd/agent/main.go
gofmt -w .
go list ./... | grep -v '/test$' | xargs go vet
```

There are no `*_test.go` tests yet. Integration checks are standalone programs in `test/` and expect the server to already be running.

```bash
go run ./test/ttft.go
go run ./test/interactive.go
go run ./test/image.go -file /path/to/image.jpg
go run ./test/memory-ttl.go
go run ./test/memory-max_turns.go
go run ./test/memory-compaction.go
go run ./test/queueing.go
```

These memory checks are easiest to understand when the server runs with a small retention budget and prompt debug dumps enabled.

Example:

```bash
MEMORY_MAX_TURNS=3 MEMORY_MAX_AGE=5s PROMPT_DEBUG_PATH=./tmp/prompt-debug go run ./cmd/agent/main.go
```

## Environment Variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8080` | WebSocket gateway port |
| `IMESSAGE_PORT` | `8090` | HTTP port for the iMessage BlueBubbles webhook listener |
| `IMESSAGE_WEBHOOK_PATH` | `/imessage/webhook` | HTTP path for incoming BlueBubbles webhooks |
| `BLUEBUBBLES_URL` | empty | BlueBubbles server base URL; enables iMessage when paired with password |
| `BLUEBUBBLES_PASSWORD` | empty | BlueBubbles server password/token used for iMessage REST API auth |
| `OLLAMA_URL` | `http://localhost:11434` | Ollama API base URL |
| `OLLAMA_MODEL` | `jaahas/qwen3.5-uncensored:4b` | Model name passed directly to Ollama |
| `SEARXNG_URL` | `http://localhost:8888` | SearXNG API base URL |
| `DISCORD_TOKEN` | empty | Enables Discord gateway |
| `WORKER_POOL_SIZE` | `1` | Broker worker count |
| `MAX_TOOL_FAILURE_RETRIES` | `3` | Max consecutive tool failures before disabling tools for the request |
| `LOG_LEVEL` | `info` | Logging verbosity |
| `MEMORY_MAX_TURNS` | `10` | Max retained session turn pairs; `0` disables the cap |
| `MEMORY_MAX_AGE` | `30m` | Max retained session age; `0` disables expiry |
| `PROMPT_DEBUG_PATH` | empty | Directory for per-request prompt markdown dumps |

## Key Files

| File | Purpose |
| --- | --- |
| `cmd/agent/main.go` | Startup wiring and shutdown |
| `internal/agent/agent.go` | Main agent loop |
| `internal/agent/summarize.go` | History compaction summarizer |
| `internal/broker/broker.go` | Request queue and worker pool |
| `internal/memory/store.go` | Session memory retention |
| `internal/memory/budget.go` | Context budget discovery |
| `internal/ollama/client.go` | Ollama HTTP client |
| `internal/tools/registry.go` | Tool schema loading and execution |
| `internal/tools/bootstrap.go` | Builtin tool wiring |
| `internal/tools/usermemory/store.go` | Persistent per-user memory store |
| `internal/tools/soulmemory/store.go` | Soul file store |
| `internal/accountlink/store.go` | Canonical account link store |
| `internal/gateway/websocket/gateway.go` | WebSocket transport |
| `internal/gateway/discord/gateway.go` | Discord transport |
| `internal/gateway/imessage/gateway.go` | iMessage BlueBubbles transport |
| `internal/debug/prompt.go` | Prompt debug dump writer |

## Code Style

- Use `gofmt`
- Keep imports grouped as stdlib, third-party, internal
- Use `%w` when wrapping errors
- Use `log.Fatal` only for startup failures in `main.go`
- Prefer `Warn` and `Error` for degraded runtime behavior instead of `panic`
- Exported types and functions should have doc comments

## Extension Patterns

### Adding a Tool

1. Add a schema file to `config/tools/<name>.md`
2. Add runtime code under `internal/tools/<name>/` if needed
3. Register the handler in `internal/tools/bootstrap.go`

### Adding a Gateway

1. Create `internal/gateway/<name>/`
2. Implement `gateway.Service`
3. Wire it in `internal/gateway/bootstrap.go`
4. Do not import concrete gateway packages directly in `cmd/agent/main.go`

### Changing Personality

- Edit `config/soul.md` directly, or
- let the agent update it through the `soul_memory` tool

Changes apply on the next request because the soul file is read fresh each time.

## Known Limitations

- Session chat history is in-process only and does not survive restart
- WebSocket gateway has no authentication layer
- Only three builtin tools ship today
- Ollama is the only LLM backend

Account-linking note:

- `config/accounts/links.json` stores canonical users and linked external accounts
- iMessage account records use normalized phone numbers or email addresses as the stable `identifier`
- iMessage `display_name` prefers a BlueBubbles-provided contact display name and falls back to the identifier when none is available

Oswald AI is a local Go agent with tools, gateways, and just enough memory to be useful.
