# AGENTS.md — Oswald AI Developer Reference

This file is the internal reference for how Oswald AI works today.

## Project Overview

Oswald AI is a pure Go application built around a single LLM gateway-backed agent loop.
It exposes that loop through Discord, a local WebSocket gateway, and an iMessage gateway backed by BlueBubbles, and ships with nine builtin model tools:

- `web.search`
- `time.current`
- `memory.save`
- `memory.search`
- `memory.list`
- `memory.forget`
- `transcript.search`
- `soul.read`
- `soul.patch`

Oswald can also expose additional tools from configured MCP servers. MCP server configurations are stored in SQLite as either global servers visible to all users or user servers visible only to one canonical user. For each visible configured MCP server, the model sees a lightweight dynamic discovery tool named `<server>.tools`; actual MCP tools remain hidden and become visible only for the active request after `<server>.tools` lists them.

Gateway-level slash commands are separate from model tools. Builtin commands include `/help`, `/connect`, `/disconnect`, `/reset`, `/privacy`, user MCP management, and admin-only user-management and global MCP commands. The nine builtin model tools remain unchanged; privacy operations are commands and are never exposed to the model as tools.

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
10. `internal/memoryformation/` — pure evidence validation, sensitivity classification, and activation policy
11. `internal/formationruntime/` — durable serialized post-turn extraction and retry worker
12. `internal/sessionruntime/` — durable proactive session-compaction planning, extraction, and serialized retry worker
13. `internal/agent/` — iterative tool-calling agent loop
14. `internal/promptbudget/` — model context budget and prompt token estimates
15. `internal/tools/` — tool registry, builtin handlers, and schema loading
16. `internal/mcp/` — optional MCP client sessions and discovered tools
17. `internal/media/` — image validation, normalization, and unsupported-file prompt notes
18. `internal/llm/` — OpenAI-compatible LLM gateway client and provider-neutral request/response schema
19. `internal/modelinfo/` — model metadata resolution with environment overrides and safe defaults
20. `internal/indexruntime/` - serialized derived-index outbox and shadow-revision worker
21. `internal/maintenanceruntime/` - serialized retention, consistency, and SQLite hygiene worker
22. `internal/privacy/` and `internal/privacyruntime/` - authenticated privacy operations and durable runtime invalidation

## Startup Flow

`cmd/agent/main.go` performs startup in this order:

1. Load environment config
2. Create the shared logger and validate required LLM gateway settings
3. Derive local HTTP and agent request timeouts from `LLM_GATEWAY_TIMEOUT`
4. Create the LLM gateway client
5. Resolve context budget from `MODEL_*` environment overrides or package defaults
6. Create the soul store
7. Open the shared SQLite-backed user-memory store and run ordered database migrations `v1` through `v11`
8. Initialize the account-link and optional MCP services
9. Start the derived-index lifecycle worker and the immediate-then-periodic maintenance worker
10. Create the command service, including `/privacy`, and start durable formation and session-compaction workers
11. Load builtin tool schemas from `data/tools/*.md`, register builtin handlers, and prepare dynamic MCP discovery tools for configured servers
12. Create the privacy invalidation bus, build enabled gateways, and start the durable invalidation dispatcher
13. Create the agent and start the broker worker pool
14. Start each gateway in its own goroutine
15. Wait for shutdown signal, then stop maintenance and privacy dispatch, drain the broker, stop index/formation/compaction workers, and close MCP clients

### Ordered Schema Migrations

Database startup serializes schema initialization and applies this exact registry in order:

| Version | Name | Purpose |
| --- | --- | --- |
| `v1` | `legacy_core_schema` | Frozen account, linked-account, challenge, memory/session, cleanup-index, and MCP baseline. |
| `v2` | `stable_tenant_profiles` | Demote legacy `system_rules`, approve migrated profile facts, and record the legacy profile ledger. |
| `v3` | `canonical_memory_formation` | Add canonical provenance/approval columns, formation candidates/evidence/jobs/audit, and backfill. |
| `v4` | `session_compaction` | Add delivery state and durable summary, source-link, and compaction-job schema. |
| `v5` | `memory_fts` | Optional FTS5 durable-memory baseline. |
| `v6` | `session_transcript_fts` | Optional FTS5 delivered-transcript baseline. |
| `v7` | `memory_operations_privacy` | Add account lifecycle fencing, forgotten-memory lifecycle fields, tenant-safe events, privacy operations, derived-index revisions/outbox, and maintenance runs. |
| `v8` | `privacy_operation_corrections` | Expand privacy operation types and correct privacy/audit retention triggers. |
| `v9` | `privacy_operation_retention` | Permit dependency-safe deletion only for completed privacy and redacted audit tombstones. |
| `v10` | `memory_event_redaction_retention` | Add event redaction/update timestamps and automatic redaction timestamping. |
| `v11` | `privacy_invalidation_outbox` | Add the leased, retryable durable runtime-invalidation outbox. |

The registry must be contiguous and have unique non-empty names and definitions. The database stores each version, name, SHA-256 checksum of the frozen definition, and application time in `schema_migration_versions`. Startup rejects unknown versions and any name/checksum drift. Exact symbolic checksums emitted by the first ordered framework for `v1` through `v6` are accepted only as known legacy values; after all rows pass validation they are removed and those frozen idempotent migrations are reapplied and recorded with definition checksums.

All pending migrations run on one connection in one `BEGIN IMMEDIATE` transaction with foreign-key actions temporarily disabled for table rebuilds. `PRAGMA foreign_key_check` must pass before commit, foreign keys are restored afterward, and any required migration or integrity failure aborts startup. Only `v5` and `v6` are optional: `ErrFTS5Unavailable` logs `database.schema.optional_unavailable`, records that frozen version, and allows startup with the affected lexical capability degraded. Other errors fail startup.

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
14. Only after successful delivery, the runtime marks the persisted turn formation-eligible, durably enqueues optional post-turn extraction, marks the turn delivered, and proactively plans any eligible session compaction

The loop is iterative, not single-pass. The model may call tools zero or more times before producing a final answer.

## Broker

The broker lives in `internal/broker/` and sits between gateways and the agent.

- Requests and commands are scheduled in FIFO lanes keyed by canonical user and session
- Only the head of each lane can occupy a worker, so independent conversations can run in parallel without concurrent work in the same session
- A fixed worker pool limits concurrent lane-head execution
- If the queue is full, the broker returns an immediate fallback response instead of blocking forever
- Shutdown rejects new work and drains all accepted lane work before returning

Relevant config:

- `WORKER_POOL_SIZE` default: `1`
- Internal request queue size: `10`

## Agent Flow

The core runtime is `(*Agent).Process()` in `internal/agent/agent.go`.

Per request it does the following:

1. Create a request-scoped timeout from `LLM_GATEWAY_TIMEOUT + 30s` (`210s` by default)
2. Inject the resolved principal into context so tools derive tenant ownership from its canonical user
3. Read `data/memory/soul/soul.md` fresh from disk
4. Build deployment policy from soul content and gateway instructions
5. Resolve the session's frozen, bounded, lower-authority tenant profile
6. Retrieve tenant-scoped lexical and semantic durable-memory candidates for the cleaned current request
7. Hybrid-rank, threshold, deduplicate, diversify, and bound recalled memories
8. Load the latest immutable structured summary and completed exchanges newer than its covered range from the active session generation
9. Pre-expose successful MCP tools from those recent turns when they remain visible and available to the current user
10. Reserve the explicitly untrusted historical summary and a minimum newest tail of complete exchanges when they fit, then select bounded recall and additional recent complete exchanges within the active model input budget
11. Build the chat message array: deployment policy as `system`, frozen tenant profile as `user`, optional generated summary as lower-authority untrusted historical reference data, recent `user`/`assistant` pairs in chronological order, and the current request plus explicitly untrusted recall as the final `user` message with any current-turn images
12. Call the LLM gateway with default-visible tools plus recent or dynamically discovered MCP tools exposed for this request
13. If the model emits tool calls:
   - execute each tool handler
   - append tool results as `tool` messages
   - repeat until no tool calls remain or the consecutive tool-failure limit is hit
14. If tool failures exhaust the retry budget, make one final model call with tools disabled
15. Persist only the cleaned final user message, final assistant reply, and compact tool-name annotations to the active session generation
16. Return the final `AgentResponse`

Multimodal request notes:

- Images are attached only to the current user turn; they are not replayed into future turns
- Session memory stays text-only; image-bearing turns are stored with a short attachment marker instead of raw image data
- Session turns are stored with a `24h` TTL and generation; complete recent exchanges from the active generation are injected automatically when budget permits
- Proactive compaction runs in the background after successful response delivery; undelivered or failed-delivery turns cannot enter summaries, transcript-search results, or compaction ranges
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
- Organized into categories like `identity`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, and `notes`
- `<id>` is now Oswald's canonical internal user ID, not a raw gateway account ID
- Eligible approved long-term identity, communication preference, durable preference, and environment facts are compiled into a deterministic profile capped at 2000 bytes
- Canonical memory writes are approved in this phase; later memory-formation policy can introduce unapproved candidates
- Tenant profiles are explicitly subordinate to deployment policy, are sent at user authority, and cannot grant capabilities, authorization, or tool access
- A profile version is frozen per canonical user and gateway session; new eligible facts appear automatically only in new, expired, or `/reset` sessions
- Legacy `system_rules` rows and filters are migrated or aliased to lower-authority `communication_preferences`
- Active durable memories are indexed by FTS5 and, when embeddings are configured, by sqlite-vec with canonical-user metadata filtering before KNN ranking
- New durable facts pass through canonical candidate states (`proposed`, `pending_confirmation`, `approved`, or `rejected`) before approved publication into the memory lifecycle (`active`, `superseded`, `expired`, or `deleted`)
- Every published memory retains exact evidence, source request/session/turn, provenance, source authority, formation mode, sensitivity, and approval metadata
- A serialized durable worker extracts only from the persisted cleaned user turn after successful response delivery; jobs use leases, idempotency keys, startup reconciliation, bounded immediate retries, and delayed dead-job redrive
- Directly stated low-sensitivity preferences, project/environment facts, and bounded task state may activate automatically after deterministic evidence validation; model inferences remain proposed
- Automatically extracted identity/contact and high-impact interaction facts require conversational confirmation in an authenticated direct session; only the exact presented phrases are accepted
- Explicit `memory.save` requests satisfy confirmation for the new fact, including sensitive data, but ambiguous replacement targets still require conversational confirmation and no candidate can publish before its source turn is persisted
- Canonical publication, supersession, evidence, audit history, profile advancement, and a durable derived-index outbox entry commit in one SQLite transaction; FTS/vector tables are derived asynchronously rather than part of canonical publication
- `memory.forget` immediately removes profile and FTS/vector serving copies and marks canonical content forgotten; maintenance scrubs that content and its linked source exchange after the configured grace period, 30 days by default
- Automatic recall combines lexical and semantic relevance with confidence, importance, recency, and source authority, then applies a measured threshold, duplicate suppression, diversity, top-K, and character caps
- Recalled memory is JSON-quoted in an explicitly untrusted lower-authority block on the current user turn; it is never added to deployment policy or persisted into session text
- Index and embedding failures degrade to whichever retrieval channel remains available without relaxing tenant filters or blocking the model response
- `memory.search` uses the same hybrid engine with a larger output cap for deeper investigation; `memory.list`, `memory.save`, and `memory.forget` remain explicit tools
- Every `memory.*` handler and `transcript.search` requires a valid authenticated request principal and derives ownership from its canonical user
- Addressed ordinary group turns continue to use the authenticated sender's private memory by explicit product decision; group chats do not create a shared memory tenant

### Canonical and Derived State

- Canonical account, memory, profile, candidate, audit, session, summary, job, privacy, and MCP rows live in SQLite and remain authoritative when retrieval indexing is unavailable
- FTS5 memory/transcript tables and sqlite-vec memory tables are rebuildable derived revisions; `derived_index_changes` is the leased, idempotent canonical-mutation outbox
- Startup bootstraps valid legacy index tables as revision one, removes legacy synchronization triggers, reconciles missing outbox entries, and then polls every 30 seconds in addition to mutation wakeups
- Canonical writes enqueue outbox changes transactionally. The serialized worker applies each change to all matching live and building revisions and retries stale canonical reads, leases, provider failures, and failed changes without weakening tenant predicates
- Rebuilds create an internally named shadow table with kind, provider, model, dimension, schema version, and monotonically increasing revision metadata
- Before publication, validation checks the physical vector dimension when applicable, exact canonical expected count, physical indexed count, canonical-user ownership joins, active/approved/unexpired memory eligibility, delivered active-generation transcript eligibility, and vector model identity
- Publication retires the old live pointer and promotes the validated shadow revision atomically. Failed validation marks only the shadow failed, so the old live revision remains available
- Maintenance removes orphan/non-canonical rows, marks missing/corrupt/coverage-mismatched live revisions unhealthy, and drops only internally generated retired or failed tables after the configured retention period
- Lexical and semantic channels fail independently. Automatic recall and `memory.search` continue with the available channel and log the unavailable channel as degraded
- During an embedding-model rebuild, semantic queries continue to embed with the old live revision's model until replacement publication; that old model must remain accessible from the provider

### Account Links

- Stored in `data/database/oswald.db`
- Maps external gateway accounts like Discord, WebSocket, and iMessage to canonical internal user IDs
- Lets persistent memory stay shared across gateways while session chat memory remains gateway/thread scoped
- `/connect` creates or confirms a hashed, expiring, one-time challenge in a direct authenticated conversation
- Confirmation atomically moves linked accounts, memories, sessions, moderation references, and re-encrypted MCP ownership before deleting the losing canonical user
- The merge also preserves profile versions/counters, candidates/evidence/relations, formation and compaction jobs/audit, summaries/source links, generation counters, privacy-safe events, and pending derived-index changes; loser-owned rows are verified absent before commit
- The profile that creates the challenge remains the canonical winner; admin state is preserved if either profile was admin
- Both participating external accounts are marked verified only after successful confirmation
- `/disconnect` requires an authenticated direct conversation and cannot remove the final account
- Admin and ban state is stored on canonical users and managed by `/admin`, `/unadmin`, `/ban`, and `/unban`
- Linking rejects banned profiles and profiles containing different accounts for the same gateway

### Privacy Commands and Erasure

`/privacy` is a gateway command family, not a model tool. Every operation requires a valid authenticated principal and a direct conversation. The service re-resolves the principal to its active canonical user; destructive storage transactions fence that whole canonical user against concurrent account merge or erasure.

Commands:

- `/privacy inspect [memories|candidates|sessions|all] [page]` returns pages of 25 lifecycle records with stable IDs and no memory/session content; memory and candidate IDs are positive decimals, while session records identify the session and generation
- `/privacy export` creates one read-transaction `oswald.user-export.v1` JSON snapshot; user MCP rows include metadata and a redacted endpoint, never encrypted URLs, headers, or credentials
- `/privacy forget-memory <id>` immediately removes one memory from profile and retrieval serving state, marks it forgotten, and schedules canonical/source-exchange scrubbing after `MEMORY_FORGOTTEN_CONTENT_GRACE` (`720h` by default)
- `/privacy delete-memory <id>` immediately scrubs one exact memory, related candidate/evidence/audit/event/relation data, derived rows, profile copies, and its linked source exchange
- `/privacy delete-candidate <id>` immediately scrubs one exact candidate, any published memory, related evidence/audit/relations, derived rows, and linked source exchanges
- `/privacy delete-session` deletes only the current session's current generation, including turns, summaries, compaction jobs, and transcript-index rows, then advances the preserved generation counter
- `/privacy delete-all-memories` requires confirmation and scrubs all memories/candidates and linked source exchanges while preserving the canonical user and unrelated sessions
- `/privacy delete-account` requires confirmation and erases the canonical user, linked accounts, memories, sessions, profiles, candidates, audit, jobs, derived work/rows, account-link state, and user-owned MCP configuration
- `/privacy confirm <code>` consumes a one-time challenge bound to the initiating normalized gateway identity; codes expire after 10 minutes and cannot be replayed or confirmed by another linked actor

Exact-ID commands reject non-positive or non-decimal IDs. Export parts are limited to 8 MiB each and 10 parts/80 MiB total. Multipart exports are raw ordered byte ranges named `.partNNN`; concatenate them byte-for-byte in filename order to reconstruct the exact JSON document.

Forget is not immediate hard deletion. It removes all serving copies immediately, but canonical content remains during the configured grace period and is scrubbed by maintenance when due. Immediate delete, session delete, confirmed delete-all, and confirmed user erasure do not use that grace period. Completed operations leave only dependency-safe content-free tombstones until retention permits removal.

Every privacy mutation durably enqueues its external-identity and session invalidation scope in the same transaction. The dispatcher reconciles expired leases at startup, polls every second, retries subscriber failures with bounded exponential backoff, and scrubs the outbox payload on completion. Account-erasure close events use a short recovery delay so the command path can deliver confirmation before immediately publishing the same invalidation; a crash still dispatches the durable event afterward. Gateways discard affected reply/session caches; account erasure also closes matching authenticated connections. If the erased external identity sends a later message, normal account resolution creates a new blank canonical user rather than restoring erased state.

Retention configuration uses positive Go durations and a positive batch size:

| Variable | Default | Purpose |
| --- | --- | --- |
| `MEMORY_FORGOTTEN_CONTENT_GRACE` | `720h` | Delay before forgotten canonical content and its source exchange are scrubbed. |
| `MEMORY_CONTENT_BEARING_AUDIT_JOB_RETENTION` | `720h` | Retain content-bearing audit/job payloads before redaction. |
| `MEMORY_CONTENT_FREE_TOMBSTONE_RETENTION` | `8760h` | Retain dependency-safe content-free tombstones. |
| `MEMORY_RETIRED_INDEX_RETENTION` | `168h` | Retain internally generated retired/failed index tables. |
| `MEMORY_SESSION_INACTIVITY` | `24h` | Active session lifetime before expiry cleanup. |
| `MEMORY_CANDIDATE_CONTENT_RETENTION` | `720h` | Retain non-published candidate content before redaction. |
| `MEMORY_SUCCESSFUL_JOB_RETENTION` | `168h` | Retain redacted successful/skipped formation and compaction jobs. |
| `MEMORY_DEAD_JOB_RETENTION` | `720h` | Retain redacted dead jobs. |
| `MEMORY_ACCOUNT_CHALLENGE_GRACE` | `24h` | Additional retention after account-link challenge expiry. |
| `MEMORY_MAINTENANCE_INTERVAL` | `1h` | Serialized sweep interval after the immediate startup sweep. |
| `MEMORY_DATABASE_OPTIMIZE_INTERVAL` | `24h` | Minimum interval between `PRAGMA optimize` runs. |
| `MEMORY_MAINTENANCE_BATCH_SIZE` | `100` | Per-category row bound for one sweep. |

Startup rejects non-positive values. Tombstone retention must be at least content-bearing retention, dead-job retention must be at least successful-job retention, and optimize interval must be at least maintenance interval.

Maintenance is serialized and runs immediately, then at `MEMORY_MAINTENANCE_INTERVAL`. It checks foreign keys before any mutation, expires inactive sessions and short-term memory, performs bounded content redaction and dependency-safe tombstone deletion, hard-deletes due forgotten content/source exchanges, removes orphan or ineligible derived rows, validates live index physical availability/corruption/exact coverage, and drops only expired internally generated retired/failed tables. Canonical retention commits before optional index/database hygiene and wakes the index worker even if later hygiene degrades.

SQLite opens with foreign keys and `secure_delete=ON`, WAL mode, `synchronous=NORMAL`, a 5-second busy timeout, immediate write locks, and a 1000-page WAL auto-checkpoint. Each sweep performs a passive WAL checkpoint, runs `incremental_vacuum(100)` only if SQLite is already in incremental auto-vacuum mode, and records/runs `PRAGMA optimize` when due. Maintenance logs only aggregate counts and durations.

### Session Chat Memory

- Stored in SQLite table `session_turns`
- Keyed by gateway-provided `SessionKey` and canonical user ID
- Stores only completed final user/assistant turn pairs
- Successful gateway delivery is recorded separately; only delivered turns are eligible for compaction and `transcript.search`
- A proactive background planner creates durable, idempotent fixed-range compaction jobs when delivered history grows past its count or prompt-budget threshold, while always leaving at least eight newest complete exchanges outside the planned range
- One serialized worker leases and retries compaction jobs, reconciles recoverable work at startup, periodically scans active sessions, and redrives bounded failed work
- Each compaction publishes a new immutable structured checkpoint containing narrative, open tasks, commitments, entities, decisions, topic tags, covered turn range, generation model/version, source digest, and ordered immutable source-turn links
- Incremental checkpoints summarize the previous checkpoint plus newly covered role-correct exchanges; published checkpoints and their source links are historical session artifacts, not durable user memories or operator instructions
- When budget permits, the agent injects the latest checkpoint only as explicitly labeled untrusted historical reference data, followed by a minimum recent verbatim tail and then any additional complete `user`/`assistant` exchanges that fit
- If the budget cannot hold all optional context, selection preserves whole exchanges, reserves the minimum recent tail before the summary, and then considers durable recall and additional history; required policy, profile, and current turn still take precedence
- Compaction does not delete covered turns. Delivered transcripts remain in SQLite and the FTS5 transcript index for the active session generation so exact episodic details remain searchable
- `transcript.search` derives canonical user, session, and generation from authenticated request context and returns bounded, role-preserving complete exchanges with session, generation, turn, creation, and delivery provenance, labeled as untrusted historical records
- Transcript search is intentionally current-session and active-generation only; it is separate from `memory.search`, which searches stable durable user facts
- Before publishing a checkpoint, the same model artifact may identify source-turn-specific durable-memory candidates from exact user evidence. These pre-compaction candidates are staged idempotently through normal validation as proposals only; compaction never directly activates memory or bypasses confirmation policy
- Recent completed exchanges newer than the latest summary boundary are replayed chronologically as complete `user`/`assistant` message pairs when budget permits, with a compact `Tools used:` annotation on the assistant message when applicable
- Successful MCP tools from the latest four exchanges are pre-exposed on the initial model call only when they remain available to the current canonical user
- Each stored turn has an optional `expires_at`, but delivered transcripts and summary sources remain retained while their matching session generation is active; startup and hourly cleanup remove them after reset or session inactivity expiry
- `/reset` advances the generation, deletes that tenant session's turns, summaries, and compaction jobs, and binds the latest tenant profile; the old transcript is no longer searchable
- Session expiry causes the next request to use a new generation, while cleanup removes inactive summaries, compaction jobs, turns, and the expired session. Generation counters are preserved so reset or expired generations are never reused
- Cleanup retains profile versions referenced by active sessions
- Tool messages and intermediate reasoning are intentionally not persisted

Prompt-budget behavior:

- The agent estimates the complete request, including tools and images, before calling the LLM gateway
- Optional durable recall is selected as whole records before session history and is omitted before required policy, profile, or current-turn content
- History selection never splits UTF-8 content or a user/assistant pair; it stops at the first complete pair that does not fit
- If required deployment policy, tenant profile, and current-turn content exceed the usable input budget, history is omitted and the request proceeds with a warning log

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
- `memory.save` — stage facts only when the current authenticated user explicitly asks Oswald to remember or correct them
- `memory.search` — run deeper tenant-scoped hybrid retrieval with confidence and provenance
- `memory.list` — inspect active stored user facts
- `memory.forget` — remove stored user facts
- `transcript.search` — search delivered role-preserving exchanges in the authenticated current session's active generation for exact episodic details
- `soul.read` — read the soul file
- `soul.patch` — add, replace, or remove one exact line in the soul file
An untrusted compacted summary, recent completed exchanges, and bounded query-relevant durable recall are injected automatically. Exact older details remain available through `transcript.search`; deeper durable retrieval and all memory mutation remain model-directed through `memory.search`, `memory.list`, `memory.save`, and `memory.forget`.
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
go run -tags sqlite_fts5 ./cmd/agent/main.go
go build -tags sqlite_fts5 -o ./tmp/main ./cmd/agent/main.go
go test -tags sqlite_fts5 ./...
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
| `internal/sessionruntime/`                     | Durable background session compaction worker |
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

- Session summaries are model-generated continuity aids and may omit or misstate details; they are untrusted, and exact delivered details are available only while the active generation's transcript is retained
- `transcript.search` is lexical FTS5 search limited to the authenticated current session's active generation; it does not search reset, expired, or other sessions
- WebSocket HMAC tokens are short-lived but do not yet support individual server-side revocation
- Only nine builtin model tools ship locally; extra tools require optional MCP integration and request-local exposure through `<server>.tools`
- MCP servers are configured dynamically in SQLite rather than hard-coded to one provider
- Runtime model access goes through an OpenAI-compatible model gateway; prompt budgeting uses OpenRouter metadata, optional `MODEL_*` overrides, or package defaults
- Application privacy deletion cannot remove copies already retained by external database backups or log sinks; operators must configure those systems' retention separately
- Privacy export delivery is capped at 10 parts of 8 MiB each (80 MiB total)
- While a replacement vector revision builds, semantic recall uses the old live revision and its embedding model; that old model must remain provider-accessible until replacement publication

Account-linking note:

- `data/database/oswald.db` stores canonical users and linked external accounts
- Existing `data/accounts/links.json` files are migrated into SQLite at startup when the database is empty
- iMessage account records use normalized phone numbers or email addresses as the stable `identifier`
- iMessage `display_name` prefers a BlueBubbles-provided contact display name and falls back to the identifier when none is available

Oswald AI is a local Go agent with tools, gateways, and just enough memory to be useful.
