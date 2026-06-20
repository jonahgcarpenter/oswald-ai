# AGENTS.md — Oswald AI Developer Reference

This file is the internal reference for how Oswald AI works today.

## Project Overview

Oswald AI is a pure Go application built around a single LLM gateway-backed agent loop.
It exposes that loop through Discord, a local WebSocket gateway, and an iMessage gateway backed by BlueBubbles, and ships with nine builtin tools:

- `web.search`
- `memory.remember`
- `memory.recall`
- `memory.forget`
- `soul.read`
- `soul.patch`
- `session.recent`
- `mcp.servers`
- `mcp.tools`

Oswald can also expose additional read-only tools discovered at startup from connected MCP servers. Today that means optional GitHub MCP integration when a GitHub personal access token is configured. Discovered MCP tools are hidden by default and become visible to the model only for the active request after `mcp.tools` lists them.

Oswald now supports multimodal user input for the active turn: text-only, image-only, and text-plus-image requests can be sent through every gateway when the active LLM gateway model route supports images.

There is no JavaScript, TypeScript, or frontend code in this repository.

## Runtime Architecture

Current layers:

1. `cmd/agent/main.go` — startup wiring
2. `internal/commands/` — shared command routing and command implementations
3. `internal/commands/accountlinking/` — canonical user identity and cross-gateway account-link commands
4. `internal/gateway/` — gateway bootstrap, shared gateway runtime, and implementations
5. `internal/routing/` — shared gateway routing policy and reply-context prompt construction
6. `internal/broker/` — request queue and worker pool
7. `internal/agent/` — iterative tool-calling agent loop
8. `internal/memory/` — in-process conversation retention and compaction
9. `internal/tools/` — tool registry, builtin handlers, and schema loading
10. `internal/mcp/` — optional MCP client sessions and discovered tools
11. `internal/media/` — image validation, normalization, and unsupported-file prompt notes
12. `internal/llm/` — OpenAI-compatible LLM gateway client and provider-neutral request/response schema
13. `internal/modelinfo/` — OpenRouter model metadata discovery with environment overrides

## Startup Flow

`cmd/agent/main.go` performs startup in this order:

1. Load environment config
2. Create the shared logger
3. Create the LLM gateway client
4. Discover context budget from OpenRouter model metadata, with `MODEL_*` environment overrides taking precedence
5. Create the soul store and persistent user-memory store
6. Create the account-link service, shared `/connect` and `/disconnect` command handler, and command router
7. Initialize optional MCP clients
8. Create the in-process conversation memory store
9. Load tool schemas from `data/tools/*.md`, register builtin handlers, and register any discovered MCP tools
10. Build enabled gateways from config
11. Create the agent
12. Start the broker worker pool
13. Start each gateway in its own goroutine
14. Wait for shutdown signal, drain the broker, and close MCP clients

## Request Lifecycle

Every request follows the same high-level path:

1. A gateway receives user input
2. The gateway normalizes text, attachments, sender metadata, and reply context
3. The gateway resolves or creates the canonical user identity through `internal/commands/accountlinking/`
4. The gateway builds a `runtime.Request` with normalized gateway facts like `IsMention`, `IsReplyToBot`, and `IsCommand`
5. `internal/gateway/runtime.Execute()` applies shared routing, command handling, fallback handling, and broker submission
6. Account-link commands are handled by the shared command router without reaching the agent loop
7. The runtime submits a `broker.Request` when the request should reach the LLM
8. A broker worker calls `(*Agent).Process()`
9. The agent builds the prompt, includes any current-turn images on the final user message, offers tools including `session.recent`, runs LLM gateway chat completions, executes tool calls if requested, and loops until the model stops calling tools
10. The final response is returned to the shared runtime
11. The gateway-specific responder sends the response back to the client, Discord channel, or iMessage chat

The loop is iterative, not single-pass. The model may call tools zero or more times before producing a final answer.

## Broker

The broker lives in `internal/broker/` and sits between gateways and the agent.

- Requests are queued through a shared buffered channel
- A fixed worker pool limits concurrent `Process()` calls
- If the queue is full, the broker returns an immediate fallback response instead of blocking forever
- Shutdown closes the queue and waits for in-flight work to finish

Relevant config:

- `WORKER_POOL_SIZE` default: `1`
- Internal request queue size: `10`

## Agent Flow

The core runtime is `(*Agent).Process()` in `internal/agent/agent.go`.

Per request it does the following:

1. Create a request-scoped timeout of `3*time.Minute`
2. Inject `SenderID` into context so tools can identify the current user
3. Read `data/memory/soul/soul.md` fresh from disk
4. Build the dynamic system prompt from:
   - soul content
   - current speaker identity when available
   - user `system_rules` memory when available
   - current date and time
5. Load retained session turns from `internal/memory`; when `LLM_GATEWAY_EMBEDDING_MODEL` is set, select only strongly relevant turns and let the model call `session.recent` for vague follow-ups
6. Compact older selected turns if the estimated prompt exceeds the active model budget
7. Build the chat message array: system prompt, selected retained history, current user prompt, and any current-turn images
8. Call the LLM gateway with all registered tools available
9. If the model emits tool calls:
    - execute each tool handler
    - append tool results as `tool` messages
    - repeat until no tool calls remain or the consecutive tool-failure limit is hit
10. If tool failures exhaust the retry budget, make one final model call with tools disabled
11. Persist only the cleaned final user message and final assistant reply to session memory
12. Return the final `AgentResponse`

Multimodal request notes:

- Images are attached only to the current user turn; they are not replayed into future turns
- Session memory stays text-only; image-bearing turns are stored with a short attachment marker instead of raw image data
- Reply context is sent directly on the current prompt, but stripped from stored session memory and semantic query text to avoid reintroducing the same quoted message later
- Attachments that fail image validation or are not supported image types are not rejected outright; gateways convert them into a short prompt note so the model knows the user attached an unsupported file

Streaming behavior:

- WebSocket clients can receive `thinking`, `content`, `status`, `tool_call`, and `tool_result` chunks while the request is running
- Discord does not stream token-by-token; it waits for the final response
- iMessage does not stream token-by-token; it waits for the final response

## Shared Routing

Gateway-neutral routing policy lives in `internal/routing/` and shared gateway execution lives in `internal/gateway/runtime/`.

- Concrete gateways own transport-specific parsing: mention detection, reply lookup, attachment downloads, account identity extraction, and response sending
- `runtime.Request` carries normalized text, channel type, mention state, reply-to-bot state, command state, current-turn images, unsupported attachment labels, and optional reply context
- `routing.Decide()` returns one of: ignore, submit to the LLM, handle a command, or send a gateway fallback response directly
- Group messages are ignored unless they mention Oswald, are replies to Oswald, or are commands addressed to Oswald
- `runtime.Execute()` applies routing decisions, executes commands, submits broker requests, and calls the gateway-specific responder
- Empty prompts with no usable images get a direct gateway fallback response
- Text-only, image-only, unsupported-attachment-only, and reply-context prompts are assembled in one shared format for every gateway
- Reply context can include quoted text, replied-to images when image slots remain, unsupported attachment labels, and unavailable-message markers
- WebSocket uses the same shared runtime as Discord and iMessage, with streaming delivered through its gateway-specific responder

## Three-Layer Memory Model

Oswald keeps three distinct memory layers.

| Layer                  | Storage                       | Purpose                                     | Mutable by agent |
| ---------------------- | ----------------------------- | ------------------------------------------- | ---------------- |
| Soul memory            | `data/memory/soul/soul.md`  | Identity, directives, personality           | Yes              |
| Persistent user memory | `data/memory/users/<id>.md` | Facts about a user that survive restart     | Yes              |
| Session chat memory    | In-process only               | Conversation history for the active session | Implicitly       |

### Soul Memory

- Stored in `data/memory/soul/soul.md`
- Read fresh on every request
- Edited through the `soul.*` tools
- Changes take effect on the next request without restart

### Persistent User Memory

- Stored in `data/memory/users/<id>.md`
- Managed by the `memory.*` tools
- Includes an intro line that identifies the current speaker across linked accounts
- Organized into categories: `identity`, `system_rules`, `preferences`, `notes`
- Uses per-user locking so different users can be updated in parallel safely
- Older flat files are migrated to categorized markdown on first recall or write
- `<id>` is now Oswald's canonical internal user ID, not a raw gateway account ID
- Only the `system_rules` category is injected automatically into the system prompt; other categories are retrieved on demand via tools

### Account Links

- Stored in `data/accounts/links.json`
- Maps external gateway accounts like Discord, WebSocket, and iMessage to canonical internal user IDs
- Lets persistent memory stay shared across gateways while session chat memory remains gateway/thread scoped
- `/connect` and `/disconnect` operate on this store before any request reaches the agent loop
- Linking can merge two canonical users when the requested external account is already attached elsewhere and the gateway sets do not conflict

### Session Chat Memory

- Stored only in memory until process exit
- Keyed by a gateway-provided `SessionKey`
- Stores only final user/assistant turn pairs
- When `LLM_GATEWAY_EMBEDDING_MODEL` is unset, retained turns are replayed using the older recency-based behavior, subject to TTL, max-turn pruning, and prompt-budget compaction
- When `LLM_GATEWAY_EMBEDDING_MODEL` is set, each stored turn gets an in-memory embedding built from the cleaned user message only; assistant text is not embedded
- Semantic retrieval embeds the cleaned current user message, strips leading reply-context wrappers, and includes up to three retained turns with cosine similarity at or above `0.70`
- No recent turn is included automatically when semantic retrieval is enabled
- The model can call `session.recent` to inspect recent completed exchanges when the current prompt is a vague follow-up and semantic context is not already present
- If query embedding fails while semantic retrieval is enabled, the agent degrades to no retained history instead of replaying all retained history
- Tool messages and intermediate reasoning are intentionally not persisted

Retention behavior:

- `MEMORY_MAX_TURNS` keeps only the newest N turn pairs when set above `0`
- `MEMORY_MAX_AGE` expires turn pairs older than the configured Go duration when set above `0`
- Pruning is destructive inside the store

Prompt-budget behavior:

- The agent estimates prompt size before calling the LLM gateway
- If selected retained history would exceed budget, the oldest selected turns are summarized into a synthetic replacement turn
- When semantic retrieval is disabled, that compacted turn is written back into session memory and gets a fresh timestamp
- When semantic retrieval is enabled, request-time compaction affects only the selected context for that request and does not overwrite unrelated retained turns
- Persisted compaction is destructive and still counts toward `MEMORY_MAX_TURNS`

## Context Budget Discovery

Context budgeting lives in `internal/memory/budget.go`.

- At startup the app queries `https://openrouter.ai/api/v1/models`
- Models are matched by `hugging_face_id` against `LLM_GATEWAY_MODEL`, first exactly and then with trimmed case-insensitive matching
- `top_provider.context_length` provides the context window when no environment override is set
- `top_provider.max_completion_tokens` provides the response reserve when no environment override is set
- `MODEL_CONTEXT_WINDOW` and `MODEL_MAX_OUTPUT_TOKENS` override discovered values field-by-field
- Max input tokens are derived as context window minus max output tokens when possible
- OpenRouter lookup still runs when overrides are set; differing discovered values are logged as override discrepancies
- If discovery and overrides do not provide a field, package defaults are used

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
- If `user_id` is present, it becomes the primary session identity and is normalized through the account-link service
- Supports text-only, image-only, and text-plus-image JSON requests
- Invalid or unsupported `images` entries are downgraded into a prompt note instead of failing the request
- Streams typed chunks during generation, then sends a final JSON response payload
- Supports `/connect` and `/disconnect` account-link commands on the same socket

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
- Attempts session resume after reconnect when Discord permits it
- Ignores bot-authored messages
- In guilds, only responds to mentions or direct replies to the bot
- In DMs, responds to any message
- Resolves Discord mentions into readable `@username` text
- Downloads supported image attachments from incoming messages and includes them on the current user turn
- Unsupported or unusable attachments are described to the model with a short prompt note instead of causing the request to fail
- Sends typing indicators while the request is running
- Splits long replies to stay under Discord's 2000-character limit
- Supports text-only, image-only, and text-plus-image messages
- Supports `/connect` and `/disconnect` account-link commands

Discord session keys use a hybrid strategy:

- DM: `discord:dm:<discord-author-id>`
- Guild channel or thread: `discord:<channel-id>:<discord-author-id>`

This prevents cross-talk between users in the same Discord channel while preserving continuity in DMs.

Reply handling:

- Replies to non-bot messages inject quoted context into the prompt
- Replies to Oswald messages can invoke Oswald without a fresh mention and inject the replied-to text as context
- Discord can fetch a referenced message from the REST API when gateway payload reply data is incomplete
- A short-lived reply index tracks recent inbound and Oswald-authored messages for reply reconstruction

### iMessage Gateway

Files:

- `internal/gateway/imessage/gateway.go`
- `internal/gateway/imessage/types.go`

Behavior:

- Listens for BlueBubbles webhook events on a dedicated HTTP port and path
- Ignores self-authored messages and payloads with neither text nor attachments
- Normalizes iMessage handles into canonical phone-number or email identifiers
- Resolves account links using contact display names when available, with identifier fallback
- In direct chats, responds to all messages; in group chats, responds only to `@oswald`, commands, or replies to Oswald
- Downloads supported image attachments from BlueBubbles by attachment GUID and includes them on the current user turn
- Unsupported or unusable attachments are described to the model with a short prompt note instead of causing the request to fail
- Sends typing indicators and replies back through the BlueBubbles REST API
- Retries BlueBubbles send failures with a fallback send method
- Looks up contact display names through BlueBubbles and caches them briefly
- Fetches replied-to message details from BlueBubbles when they are missing from the in-memory index
- Tracks a short-lived in-memory message index so reply context can be reused across follow-up messages
- Supports text-only, image-only, and text-plus-image messages
- Supports `/connect` and `/disconnect` account-link commands

iMessage session keys use a hybrid strategy:

- DM: `imessage:dm:<normalized-sender-id>`
- Group chat: `imessage:<chat-guid>:<normalized-sender-id>`

This preserves per-user continuity in direct chats while avoiding cross-talk inside group conversations.

Reply handling:

- Replies to non-bot messages inject quoted context into the prompt
- Replies to Oswald messages can invoke Oswald without a fresh mention and inject the replied-to text as context
- Cross-session replies to prior Oswald messages use quoted context rather than switching to the original sender's session

## Tools

Tools are split into schema and runtime layers.

- Schemas are loaded from `data/tools/*.md`
- Runtime handlers are wired through `internal/tools/bootstrap.go`, `internal/tools/builtin/`, and `internal/tools/mcp/`
- Additional tool definitions can be discovered dynamically from connected MCP servers

Current builtin tools:

- `web.search` — SearXNG-backed search
- `memory.remember` — store or update user facts
- `memory.recall` — retrieve stored user facts
- `memory.forget` — remove stored user facts
- `soul.read` — read the soul file
- `soul.patch` — add, replace, or remove one exact line in the soul file
- `session.recent` — read recent completed exchanges from the current in-process session
- `mcp.servers` — list connected MCP servers and read-only tool counts
- `mcp.tools` — list and request-locally expose matching read-only MCP tools from one server

`session.recent` arguments:

- `offset`: one-based recent exchange offset; `1` is the newest completed exchange
- `count`: number of exchanges to return, clamped to `1` through `3`

The tool is read-only and scoped to the current request's `session_id` from `requestctx.MetadataFromContext`.

Optional external tools:

- Read-only GitHub MCP tools are discovered at startup and registered under the `github.` namespace when `GITHUB_PERSONAL_ACCESS_TOKEN` is set
- MCP-discovered tools are not included in the default LLM tool list; `mcp.tools` exposes only returned matches for direct calls during the current request

### Tool Registry

The registry:

- loads markdown specs from disk
- converts them into LLM tool schemas
- maps tool names to handlers
- executes handlers when the model issues tool calls
- keeps builtin tools and MCP-discovered tools in the same runtime catalog

### MCP Integration

- MCP client startup lives in `internal/mcp/`
- GitHub is the only MCP server integration today
- The client connects to GitHub's streamable HTTP MCP endpoint using the configured personal access token
- Oswald only exposes tools that appear read-only; mutating GitHub tools are filtered out before registration
- MCP tools use namespaced names like `github.*` and are surfaced to the model only after request-local discovery through `mcp.tools`

### Tool Failure Handling

- Tool execution errors are converted into tool-response messages so the model can recover
- Consecutive failures are tracked per request
- Once `MAX_TOOL_FAILURE_RETRIES` is reached, the agent stops offering tools for that request and asks the model to finish without them

## LLM Gateway Integration

Files:

- `internal/llm/gateway.go`
- `internal/llm/schema.go`
- `internal/llm/types.go`
- `internal/modelinfo/`

Notes:

- The LLM gateway is the model gateway
- OpenRouter's public model catalog is used at startup for context-budget discovery
- `MODEL_*` environment overrides take precedence over discovered model metadata
- `/v1/chat/completions` is used for normal requests, tool calling, and streaming
- `/v1/embeddings` is used when `LLM_GATEWAY_EMBEDDING_MODEL` is set for semantic session-memory retrieval
- The client maps between internal app types and the gateway's OpenAI-compatible wire format
- Streaming responses accumulate both `thinking` and visible content
- Current-turn images are sent to the LLM gateway as OpenAI-compatible image URL content blocks when provided by a gateway
- Gateways normalize accepted source images into JPEG or PNG before they reach the LLM gateway

## Image Validation

Image validation is centralized in `internal/media/images.go`.

- Accepted source image formats:
  - `image/jpeg`
  - `image/png`
  - `image/webp`
  - `image/heic`
  - `image/heif`
  - `image/heic-sequence`
  - `image/heif-sequence`
- Normalized output formats sent to the LLM gateway:
  - `image/jpeg`
  - `image/png`
- Maximum images per request: `4`
- Maximum size per image: `10 MiB`
- WebSocket validates the declared MIME type and base64 payload supplied by the client
- Discord and iMessage validate attachment metadata, enforce size limits, then validate the downloaded bytes using HTTP `Content-Type`, content sniffing, and HEIC/HEIF signature detection
- Decoded images are re-encoded as PNG when transparency must be preserved; otherwise they are re-encoded as JPEG
- Any attachment that fails these checks is treated as an unsupported file and surfaced to the model via a short prompt note rather than a hard request failure

## Build, Run, and Verification

```bash
go run ./cmd/agent/main.go
go build -o ./tmp/main ./cmd/agent/main.go
go test ./...
gofmt -w .
```

There are no dedicated test files yet, but `go test ./...` should still compile every package.

## Logging

Oswald now uses structured single-line JSON logs for production and Grafana/Loki dashboards.

### Shared Envelope

Every log line should include these top-level fields:

- `ts`
- `level`
- `service`
- `log_type`
- `component`
- `event`
- `msg`

Current defaults:

- `service`: `oswald-ai`
- `level`: `debug`, `info`, `warn`, `error`
- `log_type`: `server` or `agent`

### Server vs Agent Logs

Use `server` logs for runtime infrastructure and transport behavior:

- startup and shutdown
- gateway transport
- broker queueing and workers
- provider IO
- storage and persistence
- account linking
- tool bootstrap and registry loading

Use `agent` logs for request-scoped agent execution behavior:

- `Agent.Process()` lifecycle
- context compaction
- loop iterations
- tool execution during a prompt
- final agent response completion

### Request Correlation

- Every inbound prompt gets a generated `request_id`
- `request_id` is propagated through gateway, broker, agent, tools, and provider logs
- All request-scoped logs must include `request_id`

### Agent Foundation

Every `agent` log must include:

- `request_id`
- `session_id`
- `user_id`
- `gateway`
- `model`

Use `config.Logger.Agent(...)` to attach this foundation consistently.

### Naming Conventions

Keep field names metric-friendly and stable:

- identifiers end with `_id`
- counts end with `_count`
- durations end with `_ms`
- text sizes end with `_chars`
- booleans use `is_` prefixes

Examples:

- `chat_id`
- `tool_call_count`
- `image_count`
- `duration_ms`
- `response_chars`
- `is_reply`

### Status Vocabulary

When a `status` field is used, keep it within:

- `ok`
- `error`
- `rejected`
- `retry`
- `degraded`

### Event Naming

Use stable dotted event names instead of formatting variable text into `msg`.

Examples:

- `app.start`
- `broker.request.rejected`
- `gateway.request.received`
- `provider.gateway.chat.http_error`
- `agent.request.start`
- `agent.loop.iteration`
- `agent.tool.failure`
- `agent.response.complete`

### Data Hygiene

Do not log:

- full prompt text
- full response text
- raw image bytes or base64 payloads
- full tool results
- secrets, tokens, or passwords

Prefer summaries:

- `prompt_chars`
- `response_chars`
- `thinking_chars`
- `image_count`
- `tool_call_count`
- `http_status`

### Loki Labels

Recommended low-cardinality labels:

- `service`
- `level`
- `log_type`
- `component`
- `event`

Optional:

- `gateway`

Do not use these as labels:

- `request_id`
- `session_id`
- `user_id`
- `chat_id`
- `tool_name`

### Logger API

Logging helpers live in `internal/config/logging.go`.

Preferred patterns:

- `log.Server("component")`
- `log.Agent("component", requestID, sessionID, userID, gateway, model)`
- `config.F("field_name", value)`
- `config.ErrorField(err)`

Avoid reintroducing printf-style freeform logs. New logs should be added as structured event logs so dashboards remain stable.

## Environment Variables

| Variable                   | Default                        | Purpose                                                                 |
| -------------------------- | ------------------------------ | ----------------------------------------------------------------------- |
| `PORT`                     | `8080`                         | WebSocket gateway port                                                  |
| `IMESSAGE_PORT`            | `8090`                         | HTTP port for the iMessage BlueBubbles webhook listener                 |
| `IMESSAGE_WEBHOOK_PATH`    | `/imessage/webhook`            | HTTP path for incoming BlueBubbles webhooks                             |
| `BLUEBUBBLES_URL`          | empty                          | BlueBubbles server base URL; enables iMessage when paired with password |
| `BLUEBUBBLES_PASSWORD`     | empty                          | BlueBubbles server password/token used for iMessage REST API auth       |
| `GITHUB_PERSONAL_ACCESS_TOKEN` | empty                      | Enables the GitHub MCP client and read-only `github.*` tools            |
| `LLM_GATEWAY_URL`              | `http://localhost:8080`        | LLM gateway API base URL                                                |
| `LLM_GATEWAY_MODEL`            | empty                          | Model name passed to the LLM gateway; required at startup               |
| `LLM_GATEWAY_EMBEDDING_MODEL`  | empty                          | Optional LLM gateway embedding model for semantic session-memory retrieval |
| `LLM_GATEWAY_API_KEY`          | empty                          | Optional bearer token for LLM gateway requests                          |
| `LLM_GATEWAY_VIRTUAL_KEY`      | empty                          | Optional Bifrost virtual key sent as `x-bf-vk` to the LLM gateway       |
| `MODEL_CONTEXT_WINDOW`     | `0`                            | Optional context-window override for prompt budgeting                   |
| `MODEL_MAX_OUTPUT_TOKENS`  | `0`                            | Optional output-token reserve override for prompt budgeting             |
| `SEARXNG_URL`              | `http://localhost:8888`        | SearXNG API base URL                                                    |
| `DISCORD_TOKEN`            | empty                          | Enables Discord gateway                                                 |
| `WORKER_POOL_SIZE`         | `1`                            | Broker worker count                                                     |
| `MAX_TOOL_FAILURE_RETRIES` | `3`                            | Max consecutive tool failures before disabling tools for the request    |
| `LOG_LEVEL`                | `info`                         | Logging verbosity                                                       |
| `MEMORY_MAX_TURNS`         | `10`                           | Max retained session turn pairs; `0` disables the cap                   |
| `MEMORY_MAX_AGE`           | `30m`                          | Max retained session age; `0` disables expiry                           |

## Key Files

| File                                    | Purpose                           |
| --------------------------------------- | --------------------------------- |
| `cmd/agent/main.go`                     | Startup wiring and shutdown       |
| `internal/agent/agent.go`               | Main agent loop                   |
| `internal/agent/summarize.go`           | History compaction summarizer     |
| `internal/broker/broker.go`             | Request queue and worker pool     |
| `internal/memory/store.go`              | Session memory retention          |
| `internal/memory/retrieval.go`          | Semantic session-memory selection |
| `internal/memory/compact.go`            | Retention pruning and token estimates |
| `internal/memory/budget.go`             | Context budget discovery          |
| `internal/mcp/manager.go`               | MCP client bootstrap and catalog  |
| `internal/routing/routing.go`           | Shared gateway routing policy |
| `internal/routing/types.go`             | Gateway-neutral routing types     |
| `internal/llm/gateway.go`               | LLM gateway HTTP client           |
| `internal/modelinfo/`                   | Model metadata discovery          |
| `internal/tools/registry/`              | Tool schema loading and execution |
| `internal/tools/runtime/`               | Request-local tool exposure state |
| `internal/tools/bootstrap.go`           | Tool registry assembly            |
| `internal/tools/builtin/`               | Builtin tool wiring and handlers  |
| `internal/tools/builtin/sessionhistory/` | `session.recent` runtime handler |
| `internal/tools/builtin/usermemory/store.go` | Persistent per-user memory store |
| `internal/tools/builtin/soul/store.go`  | Soul file store                   |
| `internal/commands/router.go`        | Shared command router             |
| `internal/commands/accountlinking/store.go` | Canonical account link store      |
| `internal/requestctx/requestctx.go`     | Request metadata propagation through context |
| `internal/media/images.go`              | Image normalization and validation |
| `internal/gateway/runtime/`             | Shared gateway request execution  |
| `internal/gateway/bootstrap.go`         | Gateway bootstrap                 |
| `internal/gateway/websocket/gateway.go` | WebSocket transport               |
| `internal/gateway/discord/gateway.go`   | Discord transport                 |
| `internal/gateway/imessage/gateway.go`  | iMessage BlueBubbles transport    |

## Code Style

- Use `gofmt`
- Keep imports grouped as stdlib, third-party, internal
- Use `%w` when wrapping errors
- Use `log.Fatal` only for startup failures in `main.go`
- Prefer `Warn` and `Error` for degraded runtime behavior instead of `panic`
- Exported types and functions should have doc comments

## Extension Patterns

### Adding a Tool

1. Add a schema file to `data/tools/<name>.md`
2. Add runtime code under `internal/tools/<name>/` if needed
3. Register the handler in `internal/tools/builtin/`

### Adding a Gateway

1. Create `internal/gateway/<name>/`
2. Implement `gateway.Service`
3. Normalize inbound messages into `runtime.Request`
4. Implement a gateway-specific `runtime.Responder`
5. Wire it in `internal/gateway/bootstrap.go`
6. Do not import concrete gateway packages directly in `cmd/agent/main.go`

### Changing Personality

- Edit `data/memory/soul/soul.md` directly, or
- let the agent update it through the `soul.*` tools

Changes apply on the next request because the soul file is read fresh each time.

## Known Limitations

- Session chat history is in-process only and does not survive restart
- WebSocket gateway has no authentication layer
- Only nine builtin tools ship locally; extra tools require optional MCP integration and request-local exposure through `mcp.tools`
- GitHub is the only MCP server integration today
- The OpenAI-compatible LLM gateway is the model gateway; model metadata comes from OpenRouter and optional `MODEL_*` overrides

Account-linking note:

- `data/accounts/links.json` stores canonical users and linked external accounts
- iMessage account records use normalized phone numbers or email addresses as the stable `identifier`
- iMessage `display_name` prefers a BlueBubbles-provided contact display name and falls back to the identifier when none is available

Oswald AI is a local Go agent with tools, gateways, and just enough memory to be useful.
