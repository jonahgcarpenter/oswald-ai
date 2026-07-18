# AGENTS.md — Oswald AI Developer Reference

This file is the internal reference for how Oswald AI works today.

## Project Overview

Oswald AI is a pure Go application built around a single LLM gateway-backed agent loop.
It exposes that loop through Discord, a local WebSocket gateway, and an iMessage gateway backed by BlueBubbles, and ships with eight builtin model tools:

- `web.search`
- `time.current`
- `memory.save`
- `memory.search`
- `memory.list`
- `memory.forget`
- `soul.read`
- `soul.patch`

Oswald can also expose additional tools from configured MCP servers. MCP server configurations are stored in SQLite as either global servers visible to all users or user servers visible only to one canonical user. For each visible configured MCP server, the model sees a lightweight dynamic discovery tool named `<server>.tools`; actual MCP tools remain hidden and become visible only for the active request after `<server>.tools` lists them.

Gateway-level slash commands are separate from model tools. Builtin commands include `/help`, `/connect`, `/disconnect`, and admin-only user-management commands: `/users`, `/user`, `/admin`, `/unadmin`, `/ban`, and `/unban`.

Oswald now supports multimodal user input for the active turn: text-only, image-only, and text-plus-image requests can be sent through every gateway when the active LLM gateway model route supports images.

There is no JavaScript, TypeScript, or frontend code in this repository.

## Runtime Architecture

Current layers:

1. `cmd/agent/main.go` — startup wiring
2. `internal/commands/` — shared command routing and command implementations
3. `internal/commands/accountlinking/` — canonical user identity and cross-gateway account-link commands
4. `internal/identity/` — typed request principals and identity assurance
5. `internal/commands/usermanagement/` — admin, ban, and canonical-user inspection commands
6. `internal/database/` — SQLite schema, account-link persistence, user memory tables, and sqlite-vec setup
7. `internal/gateway/` — gateway bootstrap, shared gateway runtime, and implementations
8. `internal/routing/` — shared gateway routing policy and reply-context prompt construction
9. `internal/broker/` — request queue and worker pool
10. `internal/agent/` — iterative tool-calling agent loop
11. `internal/promptbudget/` — model context budget and prompt token estimates
12. `internal/tools/` — tool registry, builtin handlers, and schema loading
13. `internal/mcp/` — optional MCP client sessions and discovered tools
14. `internal/media/` — image validation, normalization, and unsupported-file prompt notes
15. `internal/llm/` — OpenAI-compatible LLM gateway client and provider-neutral request/response schema
16. `internal/modelinfo/` — model metadata resolution with environment overrides and safe defaults

## Startup Flow

`cmd/agent/main.go` performs startup in this order:

1. Load environment config
2. Create the shared logger and validate required LLM gateway settings
3. Derive local HTTP and agent request timeouts from `LLM_GATEWAY_TIMEOUT`
4. Create the LLM gateway client
5. Resolve context budget from `MODEL_*` environment overrides or package defaults
6. Create the soul store and SQLite-backed persistent user-memory store
7. Create the account-link service and command service with `/help`, `/connect`, `/disconnect`, and admin user-management commands
8. Initialize optional MCP clients
9. Load builtin tool schemas from `data/tools/*.md`, register builtin handlers, and prepare dynamic MCP discovery tools for configured servers
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
4. The gateway creates an `identity.Principal` containing the canonical user, normalized external identity, gateway, and identity assurance
5. The gateway builds a `runtime.Request` with that principal and normalized gateway facts like `IsMention`, `IsReplyToBot`, and `IsCommand`
6. `internal/gateway/runtime.Execute()` applies shared routing, command handling, fallback handling, and broker submission
7. The runtime checks the principal's canonical user ban status before executing commands or submitting to the agent
8. Slash commands are handled by the shared command service without reaching the agent loop
9. The runtime submits a `broker.Request` carrying the same principal when the request should reach the LLM
10. A broker worker calls `(*Agent).Process()` with a typed agent request
11. The agent builds the prompt, includes any current-turn images on the final user message, offers visible tools, runs LLM gateway chat completions, executes tool calls if requested, and loops until the model stops calling tools
12. The final response is returned to the shared runtime
13. The gateway-specific responder sends the response back to the client, Discord channel, or iMessage chat

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

1. Create a request-scoped timeout from `LLM_GATEWAY_TIMEOUT + 30s` (`210s` by default)
2. Inject the resolved principal into context so tools derive tenant ownership from its canonical user
3. Read `data/memory/soul/soul.md` fresh from disk
4. Build the dynamic system prompt from:
   - soul content
   - current speaker identity when available
   - user `system_rules` memory when available
5. Load automatic retrieved context from recent SQLite-backed session turns
6. Pre-expose successful MCP tools from those recent turns when they remain visible and available to the current user
7. Estimate prompt size against the active model budget
8. Build the chat message array: system prompt, retrieved SQLite session context, current user prompt, and any current-turn images
9. Call the LLM gateway with default-visible tools plus recent or dynamically discovered MCP tools exposed for this request
10. If the model emits tool calls:
   - execute each tool handler
   - append tool results as `tool` messages
   - repeat until no tool calls remain or the consecutive tool-failure limit is hit
11. If tool failures exhaust the retry budget, make one final model call with tools disabled
12. Persist only the cleaned final user message, final assistant reply, and compact tool-name annotations to session memory
13. Return the final `AgentResponse`

Multimodal request notes:

- Images are attached only to the current user turn; they are not replayed into future turns
- Session memory stays text-only; image-bearing turns are stored with a short attachment marker instead of raw image data
- Session turns are stored with a `24h` TTL and up to four recent completed exchanges are injected automatically when budget permits
- Reply context is sent directly on the current prompt, but stripped from stored session memory and memory query text to avoid reintroducing the same quoted message later
- Attachments that fail image validation or are not supported image types are not rejected outright; gateways convert them into a short prompt note so the model knows the user attached an unsupported file
- Gateway/runtime, routing, memory, command, tool, LLM mapping, image normalization, and fake-client agent loop behavior are covered by local Go tests that do not call a live LLM

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

| Layer                  | Storage                    | Purpose                                     | Mutable by agent |
| ---------------------- | -------------------------- | ------------------------------------------- | ---------------- |
| Soul memory            | `data/memory/soul/soul.md` | Identity, directives, personality           | Yes              |
| Persistent user memory | SQLite `memory_entries`    | Facts about a user that survive restart     | Yes              |
| Session chat memory    | SQLite `session_turns`     | Conversation history for the active session | Implicitly       |

### Soul Memory

- Stored in `data/memory/soul/soul.md`
- Read fresh on every request
- Edited through the `soul.*` tools
- Changes take effect on the next request without restart

### Persistent User Memory

- Stored in `data/database/oswald.db` tables `user_memory_profiles` and `memory_entries`
- Managed by the `memory.*` tools
- Includes an intro line that identifies the current speaker across linked accounts
- Organized into categories like `identity`, `system_rules`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, and `notes`
- `<id>` is now Oswald's canonical internal user ID, not a raw gateway account ID
- Only the `system_rules` category is injected automatically into the system prompt; other categories are retrieved on demand via tools

### Account Links

- Stored in `data/database/oswald.db`
- Maps external gateway accounts like Discord, WebSocket, and iMessage to canonical internal user IDs
- Lets persistent memory stay shared across gateways while session chat memory remains gateway/thread scoped
- `/connect` creates or confirms a hashed, expiring, one-time challenge in a direct authenticated conversation
- Confirmation atomically moves linked accounts, memories, sessions, moderation references, and re-encrypted MCP ownership before deleting the losing canonical user
- The profile that creates the challenge remains the canonical winner; admin state is preserved if either profile was admin
- Both participating external accounts are marked verified only after successful confirmation
- `/disconnect` requires an authenticated direct conversation and cannot remove the final account
- Admin and ban state is stored on canonical users and managed by `/admin`, `/unadmin`, `/ban`, and `/unban`
- Linking rejects banned profiles and profiles containing different accounts for the same gateway

### Session Chat Memory

- Stored in SQLite table `session_turns`
- Keyed by gateway-provided `SessionKey` and canonical user ID
- Stores only completed final user/assistant turn pairs
- Recent completed exchanges are automatically included in the structured retrieved-memory block when budget permits, with a compact `Tools used:` annotation when applicable
- Successful MCP tools from the latest four exchanges are pre-exposed on the initial model call only when they remain available to the current canonical user
- Each stored turn has an optional `expires_at`; expired turns are deleted when recent session turns are read
- Tool messages and intermediate reasoning are intentionally not persisted

Prompt-budget behavior:

- The agent estimates prompt size before calling the LLM gateway
- If the assembled prompt exceeds the budget, the request still proceeds with a warning log; session turns are not compacted or rewritten

## Context Budget Resolution

Context budgeting lives in `internal/promptbudget/`.

- Oswald uses an OpenAI-compatible model gateway at runtime, but does not depend on live model-provider access during tests
- `MODEL_CONTEXT_WINDOW` and `MODEL_MAX_OUTPUT_TOKENS` provide explicit context-budget overrides
- Max input tokens are derived as context window minus max output tokens when possible
- If overrides do not provide a field, package defaults are used

The prompt budget is the context window minus reserves for:

- response generation
- tool overhead
- safety margin

## Gateways

Gateway bootstrap is in `internal/gateway/bootstrap.go`.

- WebSocket is always enabled and startup fails unless signed-token authentication is configured
- Discord is enabled only when `DISCORD_TOKEN` is set
- iMessage is enabled only when both `BLUEBUBBLES_URL` and `BLUEBUBBLES_PASSWORD` are set

### WebSocket Gateway

Files:

- `internal/gateway/websocket/gateway.go`
- `internal/gateway/websocket/auth.go`
- `internal/gateway/websocket/types.go`

Behavior:

- Listens on `/ws`
- Requires a valid HS256 bearer token before upgrading the connection
- Binds the connection to the token's subject and re-resolves its canonical owner before every message so account merges take effect immediately
- Accepts either plain text or JSON input
- JSON input fields:
  - `user_id`
  - `display_name`
  - `prompt`
  - `images`
- Plain-text and JSON messages both use the authenticated token subject for ownership and session identity
- If `user_id` is present, it must match the authenticated subject; attempts to switch identity close the connection
- WebSocket principals use `websocket_signed_token` assurance and are authenticated independently from account-link verification
- Browser origins must match the request host; non-browser clients may omit `Origin`
- Native browser WebSocket clients cannot set the required bearer header; this mode targets trusted service and command-line clients
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
- `time.current` — authoritative current date and time in a requested IANA timezone
- `memory.save` — explicitly store or update user facts
- `memory.search` — retrieve relevant stored user facts
- `memory.list` — inspect active stored user facts
- `memory.forget` — remove stored user facts
- `soul.read` — read the soul file
- `soul.patch` — add, replace, or remove one exact line in the soul file
Recent completed exchanges are injected automatically from session memory. Durable user-memory retrieval and saving are model-directed through `memory.search`, `memory.list`, `memory.save`, and `memory.forget`.
Current time is not injected into the system prompt; the model must call `time.current` when an answer depends on it.

Optional external tools:

- MCP server configurations are stored in SQLite with encrypted URLs and headers
- MCP-discovered tools are not included in the default LLM tool list; `<server>.tools` exposes only returned matches for direct calls during the current request
- Global MCP servers are visible to all users; user MCP servers are visible only to their owning canonical user

### Tool Registry

The registry:

- loads markdown specs from disk
- converts them into LLM tool schemas
- maps tool names to handlers
- executes handlers when the model issues tool calls
- keeps builtin tools and MCP-discovered tools in the same runtime catalog

### MCP Integration

- MCP client startup lives in `internal/mcp/`
- MCP servers are configured as HTTPS streamable HTTP/SSE endpoints in SQLite; GitHub is just one possible global server configuration
- The client connects to GitHub's streamable HTTP MCP endpoint using the configured personal access token
- Oswald only exposes tools that appear read-only; mutating GitHub tools are filtered out before registration
- MCP tools use namespaced names like `github.*` and are surfaced to the model only after request-local discovery through `<server>.tools`

### Tool Failure Handling

- Tool execution errors are converted into tool-response messages so the model can recover
- Consecutive failures are tracked per request
- Once `MAX_TOOL_FAILURE_RETRIES` is reached, the agent stops offering tools for that request and asks the model to finish without them

## Model Gateway Integration

Files:

- `internal/llm/gateway.go`
- `internal/llm/schema.go`
- `internal/llm/types.go`
- `internal/modelinfo/`

Notes:

- `LLM_GATEWAY_URL` points at an OpenAI-compatible model gateway
- `LLM_GATEWAY_VIRTUAL_KEY` can pass an optional gateway routing key when supported by the configured gateway
- `MODEL_*` environment overrides take precedence over discovered model metadata and package defaults for prompt budgeting
- `/v1/chat/completions` is used for normal requests, tool calling, and streaming
- `/v1/embeddings` is used when `LLM_GATEWAY_EMBEDDING_MODEL` is set for semantic user-memory retrieval
- The client maps between internal app types and the gateway's OpenAI-compatible wire format
- Streaming responses accumulate both `thinking` and visible content
- Current-turn images are sent to the LLM gateway as OpenAI-compatible image URL content blocks when provided by a gateway
- Gateways normalize accepted source images into JPEG or PNG before they reach the LLM gateway

## Image Validation

Image validation is centralized in `internal/media/images.go`.

- Accepted source image formats:
  - `image/jpeg`
  - `image/png`
  - `image/gif`
  - `image/webp`
  - `image/heic`
  - `image/heif`
  - `image/heic-sequence`
  - `image/heif-sequence`
- Normalized output formats sent to the LLM gateway:
  - `image/jpeg`
  - `image/png`
- Maximum images per request: `4`
- Maximum accepted source payload per image: `10 MiB`
- Maximum normalized long edge before provider submission: `2560` pixels
- Maximum normalized encoded payload before provider submission: `280 KiB`; images that still exceed this after initial normalization are downscaled further until they fit
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

## Test Standards

Tests run in GitHub Actions without project secrets or local `.env` variables, so every test must pass in a sandbox environment with no live model gateway, Discord, BlueBubbles, MCP server, SearXNG, or embedding service access.

- Use fake LLM clients, fake gateway transports, `httptest` servers, temporary directories, and isolated temporary SQLite databases
- Do not require `LLM_GATEWAY_*`, `DISCORD_TOKEN`, `BLUEBUBBLES_*`, `MCP_CONFIG_ENCRYPTION_KEY`, `SEARXNG_URL`, or model budget variables in tests
- Do not make live network calls from normal unit tests; external integrations must be mocked or guarded behind explicit opt-in checks
- Tests may validate request/response mapping and error handling, but they should not depend on a real model response
- Keep test data deterministic and avoid relying on existing files under `data/database/`, `data/accounts/`, or user memory directories

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

### Log Level Standards

Use `info` for production monitoring and audit events that should be visible during normal operation without debug logging enabled:

- application startup, shutdown, selected model, context budget, enabled gateways, enabled tools, and enabled integrations
- accepted agent requests, completed gateway commands, successful response delivery, provider completion summaries, and final agent response completion
- aggregate usage signals useful for dashboards, such as prompt counts, attachment processing counts, tool starts, token counts, latency, response sizes, and finish reasons
- durable state or security mutations, such as account linking, canonical user creation, admin changes, bans, unbans, and soul patches

Use `debug` for diagnostic details that are useful during investigation but too noisy for production monitoring:

- ignored messages, routine connection closes, typing indicator failures, reply lookup details, reply context reconstruction, and stream chunk lifecycle
- prompt/model loop internals, model-call attempts, per-iteration state, context estimate comparisons, successful tool internals, memory retrieval details, and worker processing
- attachment rejection details, image normalization metadata, individual tool/bootstrap registration details, and other high-cardinality implementation facts

Use `warn` for degraded behavior where the request may continue or recover, but operators should be able to see the condition in production:

- queue rejection, retry paths, provider stream parse/scan degradation, prompt over-budget conditions, tool execution failures, exhausted tool-failure budget, memory/session write failures, attachment fetch failures, and optional integration failures

Use `error` for failures that prevent an expected operation from completing:

- gateway send failures, account resolution failures, command execution failures, access-check failures, provider HTTP/decode failures, model-call failures, and gateway crashes

Do not promote noisy `debug` events to `info` only because they are interesting. Prefer adding a small aggregate `info` event with stable metric fields when a dashboard needs visibility.

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
- prompt budget checks
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

Use `.env.example` as the canonical configuration reference for variable names, defaults, and local setup examples. When adding or changing runtime configuration, update `.env.example` alongside `internal/config/config.go`.

## Key Files

| File                                           | Purpose                                      |
| ---------------------------------------------- | -------------------------------------------- |
| `cmd/agent/main.go`                            | Startup wiring and shutdown                  |
| `internal/agent/agent.go`                      | Main agent loop                              |
| `internal/broker/broker.go`                    | Request queue and worker pool                |
| `internal/promptbudget/`                       | Context budget and prompt token estimates    |
| `internal/mcp/manager.go`                      | MCP client bootstrap and catalog             |
| `internal/routing/routing.go`                  | Shared gateway routing policy                |
| `internal/routing/types.go`                    | Gateway-neutral routing types                |
| `internal/llm/gateway.go`                      | LLM gateway HTTP client                      |
| `internal/modelinfo/`                          | Model metadata discovery                     |
| `internal/database/`                           | SQLite schema and database helpers           |
| `internal/tools/registry/`                     | Tool schema loading and execution            |
| `internal/tools/runtime/`                      | Request-local tool exposure state            |
| `internal/tools/bootstrap.go`                  | Tool registry assembly                       |
| `internal/tools/builtin/`                      | Builtin tool wiring and handlers             |
| `internal/tools/builtin/usermemory/store.go`   | Persistent per-user memory store             |
| `internal/tools/builtin/soul/store.go`         | Soul file store                              |
| `internal/commands/service.go`                 | Shared command service                       |
| `internal/commands/parser.go`                  | Slash-command parser                         |
| `internal/commands/accountlinking/store.go`    | Canonical account link store                 |
| `internal/commands/usermanagement/commands.go` | Admin and ban command handlers               |
| `internal/identity/principal.go`                | Typed request principal and assurance        |
| `internal/requestctx/requestctx.go`            | Request metadata propagation through context |
| `internal/media/images.go`                     | Image normalization and validation           |
| `internal/gateway/runtime/`                    | Shared gateway request execution             |
| `internal/gateway/bootstrap.go`                | Gateway bootstrap                            |
| `internal/gateway/websocket/gateway.go`        | WebSocket transport                          |
| `internal/gateway/discord/gateway.go`          | Discord transport                            |
| `internal/gateway/imessage/gateway.go`         | iMessage BlueBubbles transport               |

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
3. Add the gateway's assurance value and validity mapping in `internal/identity/`
4. Resolve a typed principal and normalize inbound messages into `runtime.Request`
5. Implement a gateway-specific `runtime.Responder`
6. Wire it in `internal/gateway/bootstrap.go`
7. Add principal assurance and validity tests
8. Do not import concrete gateway packages directly in `cmd/agent/main.go`

### Changing Personality

- Edit `data/memory/soul/soul.md` directly, or
- let the agent update it through the `soul.*` tools

Changes apply on the next request because the soul file is read fresh each time.

## Known Limitations

- Session chat history is stored in SQLite `session_turns` with TTL expiry
- WebSocket HMAC tokens are short-lived but do not yet support individual server-side revocation
- Only eight builtin model tools ship locally; extra tools require optional MCP integration and request-local exposure through `<server>.tools`
- MCP servers are configured dynamically in SQLite rather than hard-coded to one provider
- Runtime model access goes through an OpenAI-compatible model gateway; prompt budgeting uses OpenRouter metadata, optional `MODEL_*` overrides, or package defaults

Account-linking note:

- `data/database/oswald.db` stores canonical users and linked external accounts
- Existing `data/accounts/links.json` files are migrated into SQLite at startup when the database is empty
- iMessage account records use normalized phone numbers or email addresses as the stable `identifier`
- iMessage `display_name` prefers a BlueBubbles-provided contact display name and falls back to the identifier when none is available

Oswald AI is a local Go agent with tools, gateways, and just enough memory to be useful.
