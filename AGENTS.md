# AGENTS.md — Oswald AI Developer Reference

This file is the internal technical reference for how Oswald AI works today. `README.md` is the user-facing product and setup guide; implementation contracts, invariants, failure behavior, and extension rules belong here. `.env.example` is the canonical configuration inventory.

## Project Overview

Oswald AI is a Go application built around a single LLM gateway-backed agent loop. SQLite and sqlite-vec use CGO-backed libraries, and Discord GIFV extraction optionally invokes external `ffmpeg` and `ffprobe` executables.
It exposes that loop through Discord, an optional Home Assistant WebSocket gateway, and an iMessage gateway backed by BlueBubbles, and ships with six builtin model tools:

- `web.search`
- `time.current`
- `user_memory_search`
- `user_memory_list`
- `session_transcript_search`
- `global_memory_search`

Oswald can also expose additional tools from configured MCP servers. MCP server configurations are stored in SQLite as either global servers visible to all users or user servers visible only to one canonical user. Every newly saved server requires an operator-provided description, which is used verbatim as the model-visible `<server>.tools` description. Actual MCP tools are hidden by default and become request-locally visible either after `<server>.tools` discovers them or when a successful tool from one of the latest four eligible exchanges remains visible and available for continuity. Servers migrated without descriptions remain model-hidden until they are updated. Remote MCP tools are not filtered for read-only behavior.

Gateway-level slash commands are separate from model tools. Builtin commands include `/help`, `/connect`, `/disconnect`, `/reset`, `/memories`, `/bootstrap`, user MCP management, and admin-only `/users`, `/user`, `/admin`, `/unadmin`, `/ban`, `/unban`, `/deleteuser`, `/global-memory`, and global MCP commands. Memory and global-memory mutations are commands and are never exposed to the model as tools.

Oswald supports multimodal user input for the active turn: text-only, image-only, and text-plus-image requests can be sent through every gateway when the active LLM gateway model route supports images.

There is no JavaScript, TypeScript, or frontend code in this repository.

## Runtime Architecture

Current layers:

1. `cmd/agent/main.go` — startup wiring
2. `internal/commands/` — shared command routing and command implementations
3. `internal/commands/bootstrap/` and `internal/commands/accountlinking/` — first-administrator bootstrap, canonical user identity, and cross-gateway account-link commands
4. `internal/identity/` — typed request principals and identity assurance
5. `internal/commands/usermanagement/` — admin, ban, and canonical-user inspection commands
6. `internal/database/` — SQLite schema, account-link persistence, user memory tables, and sqlite-vec setup
7. `internal/gateway/` — gateway bootstrap, shared gateway runtime, and implementations
8. `internal/routing/` — shared gateway routing policy and reply-context prompt construction
9. `internal/broker/` — request queue and worker pool
10. `internal/memoryformation/` — pure evidence validation, sensitivity classification, and activation policy
11. `internal/memoryextractor/` — private background user-memory model call, schema, and decoding
12. `internal/formationruntime/` — durable serialized fallback extraction and retry worker
13. `internal/sessionruntime/` — durable proactive session-compaction planning, extraction, and serialized retry worker
14. `internal/agent/` — iterative tool-calling agent loop
15. `internal/soul/` — read-only operator-managed system-prompt loader
16. `internal/promptbudget/` — model context budget and prompt token estimates
17. `internal/tools/` — tool registry, request governance, builtin handlers, and schema loading
18. `internal/mcp/` — MCP client sessions and discovered tools
19. `internal/media/` — image validation, normalization, and unsupported-file prompt notes
20. `internal/llm/` — OpenAI-compatible LLM gateway client and provider-neutral request/response schema
21. `internal/modelinfo/` — model metadata resolution with environment overrides and safe defaults
22. `internal/indexruntime/` - serialized derived-index outbox and shadow-revision worker
23. `internal/maintenanceruntime/` - serialized retention, consistency, and SQLite hygiene worker
24. `internal/runtimeinvalidation/` - in-process authorization and gateway-cache invalidation

## Startup Flow

`cmd/agent/main.go` performs startup in this order:

1. Load environment config
2. Create the shared logger and validate required LLM gateway settings
3. Prepare the configured LLM gateway endpoint and authentication
4. Create the LLM gateway client
5. Resolve context budget from `MODEL_*` environment overrides or package defaults
6. Create the soul store
7. Open the user-memory SQLite handle, install retention policy, and apply or validate the permanent ordered schema migrations
8. Open separate MCP and account-link handles to the same database; each open reruns idempotent ordered initialization under the process schema mutex
9. If no administrator exists, generate a process-local single-use bootstrap code and print instructions for claiming it from an authenticated Discord or iMessage account
10. Start the derived-index lifecycle worker and the immediate-then-periodic maintenance worker
11. Create the command service, including `/memories`, `/bootstrap`, and administrator global-memory management
12. Load the six model-visible builtin schemas from `data/tools/*.md`, construct the private background memory extractor, and construct durable formation and session-compaction workers; `mcp.Provider` creates discovery tools per request rather than registering them during bootstrap
13. Create the runtime invalidation bus and build enabled gateways
14. Create the agent, start the broker worker pool, and then start formation and compaction with the broker's low-priority model gate
15. Start each gateway in its own goroutine
16. Wait for shutdown signal, stop maintenance, drain the broker, stop index/formation/compaction workers, and close MCP clients; the current gateway interface has no graceful stop method, so gateway listeners remain live until `main` returns and deferred database closes run

### Database Migrations

Permanent migration history starts at `internal/database/migrations/v4.0.0.sql`. Files use strict `vMAJOR.MINOR.PATCH.sql` names, are embedded and ordered semantically, and contain the complete SQL for one release. `schema_migration_versions.version` is the contiguous application sequence rather than the product version; release name and SQL content are protected by a SHA-256 checksum. Applied rows must be an exact prefix of the embedded registry. Fresh databases currently receive five rows: version `1`, name `v4.0.0`; version `2`, name `v4.0.1`; version `3`, name `v4.0.2`; version `4`, name `v4.0.3`; and version `5`, name `v4.0.4`.

Migration execution runs on one connection in one `BEGIN IMMEDIATE` transaction with foreign-key actions temporarily disabled. SQL is executed directly without per-migration Go callbacks, `PRAGMA foreign_key_check` must pass before commit, and foreign keys are restored afterward. FTS5 and sqlite-vec tables are derived physical capabilities rather than canonical migration history.

Temporary compatibility for ledgerless tagged-v3.2.0 databases lives under `internal/database/legacy_migrations/`, outside permanent history. Startup accepts only the exact tagged schema as retained by upgraded installations, including its `tasks` memory category and optionally its exact sqlite-vec `memory_entry_vectors` object family at any positive dimension. It validates preserved ownership, safely drops retired `websocket` links, rejects websocket-only administrators or user MCP owners that would become inaccessible, and atomically converts to the exact v4.0.0 target while retaining only reduced account-user, supported linked-account, and MCP-server fields. All other v3 state is dropped, speaker intros are rebuilt with current gateway priority and formatting, the permanent version `1`/`v4.0.0` ledger row is recorded, and later permanent migrations are then applied normally. Unknown ledgerless schemas, old development ledgers, and checksum drift fail closed without mutation. Legacy migration assets can be removed in a later release because converted databases depend only on the permanent ledger.

The baseline has no duplicate `schema_migrations` ledger, confirmation-presentation table, general memory relation graph, memory-event or formation-audit table, persisted administrator-bootstrap state, or persisted maintenance-run history. Formation, compaction, and derived-index work share the typed `durable_jobs` table and are isolated by `job_kind`. A partial session-turn index covers only turns with no delivery outcome so timeout recovery remains bounded. Confirmation is no longer conversational; claim supersession and duplicate outcomes are represented by claim lifecycle fields. Maintenance is serialized in-process, logs aggregate results, and keeps only its process-local optimize interval marker.

## Request Lifecycle

Every request follows the same high-level path:

1. A gateway receives user input
2. The gateway normalizes text, attachments, sender metadata, and reply context
3. The gateway resolves or creates the canonical user identity through `internal/commands/accountlinking/`
4. The gateway creates an `identity.Principal` containing the canonical user, normalized external identity, gateway, and identity assurance
5. The gateway builds a `runtime.Request` with that principal and normalized gateway facts like `IsMention` and `IsReplyToBot`; command-attempt status is derived by the shared runtime from normalized text
6. `internal/gateway/runtime.Execute()` applies shared routing, command handling, fallback handling, and broker submission
7. The runtime checks the principal's canonical user ban status before executing commands or submitting to the agent
8. Slash commands are handled by the shared command service without reaching the agent loop
9. The runtime submits a `broker.Request` carrying the same principal when the request should reach the LLM
10. A broker worker calls `(*Agent).Process()` with a typed agent request
11. The agent builds the prompt, includes any current-turn images on the final user message, offers visible tools, runs LLM gateway chat completions, executes tool calls if requested, and loops until the model stops calling tools
12. The final response is returned to the shared runtime
13. The gateway-specific responder sends the response back to the client, Discord channel, or iMessage chat
14. Only after successful delivery, the runtime records `delivered_at`, durably enqueues fallback extraction, and proactively plans any eligible session compaction

The loop is iterative, not single-pass. The model may call tools zero or more times before producing a final answer.

## Broker

The broker lives in `internal/broker/` and sits between gateways and the agent.

- Requests and commands are scheduled in FIFO lanes keyed by canonical user and session
- Only the head of each lane can occupy a worker, so independent conversations can run in parallel without concurrent work in the same session
- A fixed worker pool limits concurrent lane-head execution
- The additional queued-work allowance is `10`; the global outstanding cap is `10 + WORKER_POOL_SIZE`, including active work
- If the outstanding cap is reached, the broker returns an immediate fallback response instead of blocking forever
- Shutdown rejects new work, cancels active and queued agent contexts, and drains accepted lane operations before returning

Relevant config:

- `WORKER_POOL_SIZE` default: `1`
- Additional queued-work allowance: `10`

## Agent Flow

The core runtime is `(*Agent).Process()` in `internal/agent/agent.go`.

Per request it does the following:

1. Use the broker-owned lifecycle context so shutdown cancellation propagates through model and tool work without imposing an Oswald generation deadline
2. Inject the resolved principal into context so tools derive tenant ownership from its canonical user
3. Read `data/memory/soul/soul.md` fresh from disk
4. Build deployment policy from soul content and gateway instructions
5. Resolve the session's frozen, bounded, lower-authority tenant profile
6. Retrieve tenant-scoped lexical and semantic durable-memory candidates for the cleaned current request
7. Hybrid-rank, threshold, deduplicate, diversify, and bound recalled memories
8. Load the latest immutable structured summary and completed exchanges newer than its covered range from the active session generation
9. Pre-expose successful MCP tools from those recent turns when they remain visible and available to the current user
10. Reserve the explicitly untrusted historical summary and up to two newest complete exchanges within the 25%-of-input, 2,000-to-8,000-token recent-tail budget, then select bounded recall and additional recent complete exchanges within the active model input budget
11. Build the chat message array: deployment policy as `system`, frozen tenant profile as `user`, optional generated summary as lower-authority untrusted historical reference data, recent `user`/`assistant` pairs in chronological order, and the current request plus explicitly untrusted recall as the final `user` message with any current-turn images
12. Call the LLM gateway with default-visible tools plus recent or dynamically discovered MCP tools exposed for this request
13. If the model emits tool calls:
   - authorize every call against the exact catalog advertised for that model iteration
   - block exact request-local duplicates using a hash of the tool name and normalized canonical arguments
   - execute allowed handlers and classify successful results as productive or unproductive
   - append exactly one correlated `tool` result for every declared call, including blocked calls in multi-call batches
   - retire only a tool that exhausts its configured execution, failure, or unproductive-result allowance
   - repeat until no tool calls remain or a global tool-governance limit is hit
14. If the global execution, tool-iteration, or consecutive-failure budget is exhausted, make one final model call with all tools disabled
15. Persist only the cleaned final user message, final assistant reply, and compact tool-name annotations to the active session generation
16. Return the final `AgentResponse`

Multimodal request notes:

- Images are attached only to the current user turn; they are not replayed into future turns
- Session memory stays text-only; image-bearing turns are stored with a short attachment marker instead of raw image data
- Session expiry is controlled by `MEMORY_SESSION_INACTIVITY` (`24h` by default); complete recent exchanges from the active generation are injected automatically when budget permits
- Proactive compaction runs in the background after successful response delivery; undelivered or failed-delivery turns cannot enter summaries, transcript-search results, or compaction ranges
- Reply context is sent directly on the current prompt, but stripped from stored session memory and memory query text to avoid reintroducing the same quoted message later
- Attachments that fail image validation or are not supported image types are not rejected outright; gateways convert them into a short prompt note so the model knows the user attached an unsupported file
- On the recognized Ollama model-runner resource failure, model submission retries up to five times with progressively downscaled current-turn images; exhaustion returns a fixed image-size fallback, while unrelated provider failures do not use this path
- Gateway/runtime, routing, memory, command, tool, LLM mapping, image normalization, and fake-client agent loop behavior are covered by local Go tests that do not call a live LLM

Streaming behavior:

- Home Assistant receives correlated `thinking`, `content`, `tool_call`, and `tool_result` frames while the request is running
- Discord does not stream token-by-token; it waits for the final response
- iMessage does not stream token-by-token; it waits for the final response

## Shared Routing

Gateway-neutral routing policy lives in `internal/routing/` and shared gateway execution lives in `internal/gateway/runtime/`.

- Concrete gateways own transport-specific parsing: mention detection, reply lookup, attachment downloads, account identity extraction, and response sending
- `runtime.Request` carries normalized text, channel type, mention state, reply-to-bot state, current-turn images, unsupported attachment labels, and optional reply context; the runtime derives command-attempt state
- `routing.Decide()` returns one of: ignore, submit to the LLM, handle a command, or send a gateway fallback response directly
- Ordinary group messages are ignored unless they mention or reply to Oswald. Slash-command attempts in groups require a mention; an unmentioned group command is ignored even when syntactically valid
- Conversation scope is not forwarded to command handlers, middleware, or fence resolvers, so an admitted group command executes identically to the same private command
- `runtime.Execute()` applies routing decisions, executes commands, submits broker requests, and calls the gateway-specific responder
- Empty prompts with no usable images get a direct gateway fallback response
- Text-only, image-only, unsupported-attachment-only, and reply-context prompts are assembled in one shared format for every gateway
- Reply context can include quoted text, replied-to images when image slots remain, unsupported attachment labels, and unavailable-message markers
- Home Assistant uses the same shared runtime as Discord and iMessage, with streaming delivered through its gateway-specific responder

## Four-Layer Memory Model

Oswald keeps four distinct memory layers.

| Layer                  | Storage                    | Purpose                                     | Mutable by agent |
| ---------------------- | -------------------------- | ------------------------------------------- | ---------------- |
| Soul memory            | `data/memory/soul/soul.md` | Identity, directives, personality           | No               |
| Global memory          | SQLite `global_memories`   | Administrator-curated facts about Oswald shared across tenants | No |
| Persistent user memory | SQLite `memory_entries`    | Facts about a user that survive restart     | Yes              |
| Session chat memory    | SQLite `session_turns`     | Conversation history for the active session | Implicitly       |

### Soul Memory

- Stored in `data/memory/soul/soul.md`
- Read fresh on every request
- Used as the base system prompt, followed only by trusted gateway runtime instructions at the same authority
- Changed only through operator filesystem or deployment access; model tools cannot read or mutate it
- Manual changes take effect on the next request without restart and affect every user

### Global Memory

- Global memory is factual lower-authority reference data, separate from soul policy and personality
- Canonical rows live in the administrator-curated `global_memories` table; global memory is never learned automatically from MCP results, user prompts, or model tool calls
- Administrators manage the store with `/global-memory add <memory text>`, `/global-memory list [page]`, and `/global-memory forget <id>`
- Add trusts the administrator-provided content, normalizes it, enforces the 1,000-rune storage bound, and rejects an exact normalized duplicate. It does not filter credentials or instruction-like text. Forget hard-deletes the canonical row and enqueues deletion from derived indexes; there is no staged, evidence-evolving, supersession, or post-delivery publication lifecycle
- `global_memory_search` is the sole model-visible global-memory tool and requires a valid authenticated tenant principal. There are no model-visible global-memory add, save, list, or forget tools
- Global memory is not injected into prompts automatically. The model calls `global_memory_search` when a request concerns Oswald's implementation, hardware, deployment, version, architecture, configuration, capabilities, or similar deployment facts
- Search hybrid-ranks lexical FTS5 and semantic sqlite-vec results. Either channel may degrade independently; if neither derived channel is available, a bounded scan of canonical `global_memories` provides fallback results
- Search returns matching records directly as newline-delimited JSON containing ID, memory text, score, and retrieval sources, without an additional heading or authority-warning wrapper
- Semantic retrieval is enabled only when `LLM_GATEWAY_EMBEDDING_MODEL` is configured. Without it, lexical retrieval and bounded canonical fallback remain available

### Persistent User Memory

- Stored in `data/database/oswald.db`; the speaker intro lives on `account_users` and durable user facts remain tenant-owned memory claims
- Retrieved by `user_memory_search` and `user_memory_list`; mutation is owned by background formation and authenticated memory commands rather than primary-agent tools
- Includes an intro line that identifies the current speaker across linked accounts
- Organized into categories like `identity`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, and `notes`
- `<id>` is now Oswald's canonical internal user ID, not a raw gateway account ID
- Eligible approved long-term identity, communication preference, durable preference, and environment facts are compiled into a deterministic profile capped at 2000 bytes
- Canonical memory publication occurs only from policy-approved background candidates. The primary agent has no user-memory mutation tools. Exact explicit remember wording still supplies a deterministic confidence floor to background evaluation but never bypasses grounding, threshold, authority, temporary-state, or supersession policy
- Tenant profiles are explicitly subordinate to deployment policy, are sent at user authority, and cannot grant capabilities, authorization, or tool access
- A profile version is frozen per canonical user and gateway session; new eligible facts appear automatically only in new, expired, or `/reset` sessions
- Legacy `system_rules` rows and filters are migrated or aliased to lower-authority `communication_preferences`
- Active durable memories are indexed by FTS5 and, when embeddings are configured, by sqlite-vec with canonical-user metadata filtering before KNN ranking
- Candidate policy state is semantic and immutable after same-turn reconciliation: `proposed` means sound but below confidence `0.35`, `approved` means sound at or above that threshold, and `rejected` means unsound regardless of confidence. A non-null `published_memory_id` means published; an approved candidate without one is blocked by conflict. Published memories use the committed `active`, `superseded`, and `expired` lifecycle, while deletion is immediate and physical
- `memory_candidates` is the one-row-per-extracted-observation evidence ledger. Published candidates link to their consolidated `memory_entries` row; evidence count, source request/session/generation, correlation, representative evidence, and authority are derived through candidate and source-turn data. `memory_entries` retains only compact serving and conflict metadata such as confidence, importance, strongest provenance, and sensitivity. Candidate rows are directly deleted when their memory is deleted or retention expires
- The serialized post-delivery extractor in `internal/memoryextractor/` exposes only one private `user_memory_save`-shaped schema and forces one call with standard `tool_choice = "required"`, parallel tool calls disabled, temperature `0`, and a 2,048-token output cap; the batch contains at most five candidates. This schema is not loaded into the primary tool registry. Its prompt names all required fields, includes complete and empty JSON examples, and enumerates exact dotted claim-slot namespaces. Valid siblings are evaluated independently, malformed items are dropped and counted, and structurally invalid output receives one bounded retry before terminal skip under a stable non-sensitive reason code. Valid empty batches and policy-rejected candidates do not receive a corrective model call. The first decoded batch and its submitted/malformed counts are persisted for idempotent replay; legacy artifacts without those counts remain readable. Non-retryable provider 4xx responses remain terminally skipped
- Formation jobs use renewable exact-token leases with a five-minute initial duration and idempotency keys. They receive up to five immediate operational attempts and at most three delayed redrives only when the stored error code is explicitly transient; these retries recover calls that did not produce a persisted extractor batch and never ask the model to correct a policy result. The one structured-output retry is tracked durably and independently, and its invalid claim is refunded from the operational attempt budget. Foreground preemption before Bifrost accepts an async job durably defers work without consuming either budget; cancellation after acceptance consumes an operational attempt because Bifrost may continue the remote job. Operational redrive never restores a consumed structured-output retry. Candidate proposal, publication, and completion require a transactionally checked live exact lease; retry and terminal skip may release a naturally expired lease only while its exact token remains stored. Startup reconciliation backfills missing jobs only for delivered turns created during the previous 24 hours
- Unambiguous exact first-person evidence spans from longer turns and whole-turn direct statements become active when confidence is at least `0.35`; evidence must begin with a first-person marker and express a positive, current, non-modal fact. A lexically grounded canonical statement is retained. If only that model paraphrase check fails, policy substitutes a deterministic exact-evidence wrapper while grounding direct claim values against evidence alone; competing or ambiguous facts still fail closed. Rune-safe checks include quote state and containing-sentence context without splitting common abbreviations or decimals. A positive independent evidence clause may remain eligible when surrounding text asks a question, while an interrogative evidence clause remains ineligible. Direct identity facts receive deterministic minimum importance `3`. Quoted/reported, negative, obsolete, hypothetical/conditional, third-party-centered, publicly attributed, and instruction/policy/capability-like text fails closed in every mode. User-centered relationship identity requires explicit `is named`/`name is` grammar and a compatible relationship name/identity slot. Model inference remains whole-turn-only, positive, cautious, user-centered, and lexically relevant
- Model inference uses exact whole-turn evidence but may express a cautious implication not lexically present in the turn. Third-party, public, tool-derived, quoted, hypothetical, and instruction-like content remains disallowed
- Stable category-compatible `(claim_slot, claim_value)` identity consolidates supporting turns into one memory; user-memory tables do not store derived statement or claim keys. Equivalent background candidates from the same source turn and exact evidence reconcile only through equal slot/value identity and never double-count. Different values on a single-valued slot conflict, while `.fact` slots remain multi-valued and identify independent observations by value. Independent evidence combines confidence using bounded noisy-OR; correlated same-session inferred evidence is discounted, retries are idempotent, and stronger direct evidence upgrades authority without losing prior evidence
- User-memory candidates and entries store `provenance_type`, not a redundant source-authority column. Authority is derived deterministically as `user_statement` to `user_direct`, `model_inference` to `model`, and otherwise unknown for serving; rejected external provenance types may remain in the candidate audit ledger but never publish. Inferred memories are active only for confidence-tiered query-relevant recall and are explicitly labeled `uncertain_inference`; they cannot enter a tenant profile until direct evidence upgrades provenance
- Sensitivity is retained independently from confidence and does not trigger a conversational approval prompt. Every extracted candidate remains source-turn-fenced, requires stable `claim_slot`/`claim_value`, and corrections supersede atomically only when ordinary authority and confidence comparison permits
- Conflicting claim values use monotonic authority and confidence: stronger evidence may supersede a weaker active claim, while weak inference cannot replace a stronger direct fact
- Candidate insertion or same-turn reconciliation, canonical publication or reinforcement, supersession, profile advancement, and a durable derived-index outbox entry commit in one lease-fenced SQLite transaction. Successful approved proposals therefore cannot remain unpublished unless blocked by a stronger conflicting claim; FTS/vector tables are derived asynchronously rather than part of canonical publication
- Inactive candidate history is directly hard-deleted by bounded maintenance without an intermediate retention stage
- `/memories forget <id>` atomically hard-deletes the selected memory, its candidates, frozen profile references, and physical/queued derived-index state. Its source conversation remains intact and may teach the same fact again later
- Automatic recall combines lexical and semantic relevance with confidence, importance, recency, and provenance-derived authority, then applies a measured threshold, duplicate suppression, diversity, top-K, and character caps
- Recalled memory is JSON-quoted in an explicitly untrusted lower-authority block on the current user turn; it is never added to deployment policy or persisted into session text
- Index and embedding failures degrade to whichever retrieval channel remains available without relaxing tenant filters or blocking the model response
- `user_memory_search` uses the same hybrid engine with a larger output cap for deeper investigation; `user_memory_search`, `user_memory_list`, and `session_transcript_search` are the model-directed user-memory retrieval tools
- Every model-visible user-memory handler and `session_transcript_search` requires a valid authenticated request principal and derives ownership from its canonical user
- Addressed ordinary group turns continue to use the authenticated sender's private memory by explicit product decision; group chats do not create a shared memory tenant

### Canonical and Derived State

- Canonical account, global-memory, user-memory, profile, candidate, session, summary, job, and MCP rows live in SQLite and remain authoritative when retrieval indexing is unavailable
- FTS5 and sqlite-vec tables are rebuildable derived revisions. Index kinds are `memory_fts`, `transcript_fts`, `memory_vector`, `global_memory_fts`, and `global_memory_vector`; `durable_jobs` rows with `job_kind = 'derived_index'` form the leased, idempotent canonical-mutation outbox
- Global-memory outbox rows use `entity_kind = 'global_memory'` and a `NULL` canonical user because the records are shared. Private memory and transcript outbox rows require a canonical user and remain tenant-fenced throughout indexing and retrieval
- Startup removes obsolete fixed index artifacts, creates internally named generated revisions through the index lifecycle worker, reconciles missing outbox entries, and then polls every 30 seconds in addition to mutation wakeups
- Succeeded derived-index history is pruned after `MEMORY_SUCCESSFUL_JOB_RETENTION`, except for one successful upsert receipt per still-live canonical entity. Reconciliation recognizes that receipt, preventing unchanged live state from recreating historical work
- Canonical writes enqueue outbox changes transactionally. The serialized worker applies each change to all matching live and building revisions and retries stale canonical reads, leases, provider failures, and failed changes without weakening tenant predicates
- Rebuilds create an internally named shadow table with kind, model, dimension, schema version, and monotonically increasing revision metadata; table names are globally unique and generated names must exactly encode their recorded kind and revision before publication or cleanup
- Before publication, validation checks the physical vector dimension when applicable, exact canonical expected count, physical indexed count, canonical-user ownership joins, active/approved/unexpired memory eligibility, delivered active-generation transcript eligibility, and vector model identity
- Publication retires the old live pointer and promotes the validated shadow revision atomically. Failed validation marks only the shadow failed, so the old live revision remains available
- Maintenance removes orphan/non-canonical rows, marks missing/corrupt/coverage-mismatched live revisions unhealthy, and drops only internally generated retired or failed tables after the configured retention period; maintenance runs are logged rather than stored as canonical rows
- Lexical and semantic channels fail independently. Automatic recall and `user_memory_search` continue with the available channel and log the unavailable channel as degraded
- During an embedding-model rebuild, semantic queries continue to embed with the old live revision's model until replacement publication; that old model must remain accessible from the provider
- The derived-index worker probes the configured embedding dimension until its first success, caches it for the process lifetime, and requires an Oswald restart to recognize gateway-side embedding route changes

### Account Links

- Stored in `data/database/oswald.db`
- Maps external gateway accounts like Discord, Home Assistant, and iMessage to canonical internal user IDs
- Lets persistent memory stay shared across gateways while session chat memory remains gateway/thread scoped
- `/connect` creates or confirms a hashed, expiring, one-time challenge in a direct authenticated conversation
- Confirmation atomically moves linked accounts, memories, sessions, moderation references, and re-encrypted MCP ownership before deleting the losing canonical user
- The merge preserves consolidated session rows and their profile/generation high-water, candidates/evidence, formation and compaction jobs, summaries/source links, and pending derived-index changes; loser-owned rows are verified absent before commit
- The profile that creates the challenge remains the canonical winner; admin state is preserved if either profile was admin
- Both participating external accounts are marked verified only after successful confirmation
- `/disconnect` requires an authenticated identity and cannot remove the final account
- Admin and ban state is stored on canonical users and managed by `/users`, `/user`, `/admin`, `/unadmin`, `/ban`, `/unban`, and `/deleteuser`
- Linking rejects banned profiles and profiles containing different accounts for the same gateway
- `/connect`, `/disconnect`, `/memories`, and `/bootstrap` require an authenticated identity. `/bootstrap <code>` accepts Discord, iMessage, or Home Assistant principals, promotes the currently resolved canonical account only while no administrator exists, and consumes its process-local code after a successful update. Restart generates a replacement code only while no administrator exists. Group slash commands still require an Oswald mention, and bootstrap codes may be visible to other group members.

### Memory Commands and Deletion

`/memories` is a gateway command family, not a model tool. Every operation requires a valid authenticated principal and works in private or group conversations. The service re-resolves the principal to its active canonical user; destructive storage transactions fence that whole canonical user against concurrent account mutation.

Commands:

- `/memories list` returns every active, unexpired memory as UTF-8 text attachment data containing stable ID, category, and statement. The command is not capped at the model tool's 25-memory limit; large payloads are split at UTF-8 boundaries within the shared 10-part/80 MiB command-attachment limits
- `/memories forget <id>` immediately hard-deletes one memory, its candidates, profile copies, physical derived rows, and pending derived-index state. Source conversation turns and summaries remain intact
- `/memories forget all` immediately hard-deletes all memories, candidates, user MCP configuration, session turns, summaries, formation/compaction work, profiles, and derived serving rows. Every session generation is reset while the canonical account, linked identities, admin/ban state, and authentication clients remain

Exact IDs reject non-positive or non-decimal values. A complete list that exceeds the UTF-8-safe multipart attachment limit fails rather than returning a partial result.

Exact-ID and forget-all deletion are physical row deletions in the command transaction. SQLite uses `secure_delete=ON`; WAL files and external backups remain subject to ordinary SQLite checkpointing and operator backup retention.

Runtime invalidation is in-process and transport-neutral. Account disconnect, user deletion, and forget-all clear matching gateway caches and can close matching Home Assistant connections.

Retention configuration uses positive Go durations and a positive batch size:

| Variable | Default | Purpose |
| --- | --- | --- |
| `MEMORY_RETIRED_INDEX_RETENTION` | `168h` | Retain internally generated retired/failed index tables. |
| `MEMORY_SESSION_INACTIVITY` | `24h` | Active session lifetime before expiry cleanup. |
| `MEMORY_PENDING_DELIVERY_TIMEOUT` | `15m` | Mark a persisted turn with no delivery outcome as terminally failed so it cannot indefinitely block compaction. |
| `MEMORY_SUCCESSFUL_JOB_RETENTION` | `168h` | Retain successful/skipped formation and compaction jobs. |
| `MEMORY_DEAD_JOB_RETENTION` | `720h` | Retain permanently failed formation and compaction jobs. |
| `MEMORY_ACCOUNT_CHALLENGE_GRACE` | `24h` | Additional retention after account-link challenge expiry. |
| `MEMORY_MAINTENANCE_INTERVAL` | `1h` | Serialized sweep interval after the immediate startup sweep. |
| `MEMORY_DATABASE_OPTIMIZE_INTERVAL` | `24h` | Minimum interval between `PRAGMA optimize` runs. |
| `MEMORY_MAINTENANCE_BATCH_SIZE` | `100` | Per-category row bound for one sweep. |

Fallback memory extraction and session-compaction model calls are always enabled and share one broker-owned low-priority permit. They run only with no active or queued foreground work, and accepted foreground work cancels and durably defers the background call without consuming its provider retry budget.

Startup rejects non-positive values. Dead-job retention must be at least successful-job retention, and optimize interval must be at least maintenance interval.

Maintenance is serialized and runs immediately, then at `MEMORY_MAINTENANCE_INTERVAL`. It checks foreign keys before any mutation, terminally fails stale turns that still have neither delivery outcome, expires inactive sessions and short-term memory, directly deletes stale candidates and terminal jobs, prunes derived-index history while retaining live receipts, removes orphan or ineligible derived rows, validates live index physical availability/corruption/exact coverage, and drops only expired internally generated retired/failed tables. A terminal no-artifact compaction job remains as the failed-contract suppression receipt while its exact session generation is active, then ordinary session cleanup removes it. All categories are batch-bounded and reported only as aggregate counts. Canonical retention commits before optional index/database hygiene and wakes the index worker even if later hygiene degrades. A genuine late successful delivery clears a timeout failure before formation and compaction eligibility is restored.

SQLite opens with foreign keys and `secure_delete=ON`, WAL mode, `synchronous=NORMAL`, a 5-second busy timeout, immediate write locks, and a 1000-page WAL auto-checkpoint. Each sweep performs a passive WAL checkpoint, runs `incremental_vacuum(100)` only if SQLite is already in incremental auto-vacuum mode, and records/runs `PRAGMA optimize` when due. Maintenance logs only aggregate counts and durations.

Operator backup contract: `data/database/oswald.db` is canonical, but a live file copy is not safe in WAL mode. Use SQLite's online `.backup` command, or stop Oswald and copy the database with its `-wal` and `-shm` companions. Keep the exact `MCP_CONFIG_ENCRYPTION_KEY` separately because restored MCP ciphertext requires it. Restore only while Oswald is stopped, remove stale destination WAL/SHM files, and require `PRAGMA integrity_check` to return `ok` plus an empty `PRAGMA foreign_key_check` before startup. External backups and log sinks require independent access, retention, and deletion controls.

### Session Chat Memory

- Stored in SQLite table `session_turns`
- Keyed by gateway-provided `SessionKey` and canonical user ID
- Stores only completed final user/assistant turn pairs
- Successful gateway delivery is recorded separately; only delivered turns are eligible for compaction and `session_transcript_search`
- Every persisted foreground turn carries an immutable deterministic completed-request pressure snapshot. The snapshot remains inert while delivery is pending and becomes planner-visible only after successful delivery. When pressure reaches the same model input boundary used by foreground prompt assembly, the planner creates a durable campaign covering every eligible exchange except a contiguous newest tail of at most two complete exchanges. That tail is bounded to 25% of the input limit, clamped to 2,000 through 8,000 estimated tokens; if the newest exchange does not fit, no verbatim tail is retained. Provider-reported usage remains telemetry rather than policy input
- A campaign target is stable and may be processed through multiple fixed-range jobs of at most 64 exchanges. Successful partial checkpoints replan until the target is covered even when the first checkpoint lowers current pressure. Model and `session-summary-v1` generator contracts are pinned to each job; at most one queued/running/retry job exists per tenant session generation, and an unresolved or terminally failed contract suppresses overlapping ranges until the configured model or generator version changes. A complete exchange that cannot fit with the previous checkpoint creates a durable `uncompactable_complete_exchange` receipt without a provider call
- One serialized worker uses renewable unique-token leases and retries compaction jobs, reconciles recoverable work at startup, periodically scans active sessions, and redrives only jobs whose stored error code is explicitly transient. Its model calls share the broker's foreground-preemptible low-priority permit. The private extractor advertises only `session_summary_save`, requires exactly one call, disables parallel calls, uses temperature `0`, and caps output at the smaller of 4,096 tokens and the resolved model output reserve. Its provider-visible schema intentionally avoids string-length and array-cardinality keywords that can expand or break llama.cpp grammars; Oswald still enforces all canonical size/count limits before persistence. Structurally invalid output receives one durable retry before terminal skip, permanent provider 4xx responses skip immediately except for `408`, `425`, and `429`, and valid policy-rejected candidates do not trigger regeneration
- Low-priority gate refusal before provider submission durably defers compaction without consuming an attempt. Foreground cancellation after provider submission consumes an operational attempt, so repeated preemption is bounded by the same three-attempt and three-redrive transient budget rather than producing an unbounded call loop
- Each compaction publishes a new immutable structured checkpoint containing narrative, open tasks, commitments, entities, decisions, topic tags, covered turn range, and ordered source-turn IDs stored as JSON on the checkpoint
- Incremental checkpoints summarize the previous checkpoint plus newly covered role-correct exchanges; published checkpoints and their source links are historical session artifacts, not durable user memories or operator instructions
- When budget permits, the agent injects the latest checkpoint only as explicitly labeled untrusted historical reference data, followed by the token-bounded zero-to-two-exchange recent tail and then any additional complete `user`/`assistant` exchanges that fit
- If the budget cannot hold all optional context, selection preserves whole exchanges, reserves the token-bounded recent tail before the summary, and then considers durable recall and additional history; required policy, profile, and current turn still take precedence
- Compaction does not delete covered turns. Delivered transcripts normally remain in SQLite and the FTS5 transcript index for the active session generation so exact episodic details remain searchable, except when forget-all or account deletion resets all user data
- `session_transcript_search` derives canonical user, session, and generation from authenticated request context and returns bounded, role-preserving complete exchanges with session, generation, turn, creation, and delivery provenance, labeled as untrusted historical records
- Transcript search is intentionally current-session and active-generation only; it is separate from `user_memory_search`, which searches stable durable user facts
- Before publishing a checkpoint, the same model artifact may identify source-turn-specific durable-memory candidates with full claim identity from exact user evidence. They use the same soundness and confidence policy as post-delivery extraction; approved candidates publish only under the exact live compaction lease and delivered active-generation source-turn fence
- Recent completed exchanges newer than the latest summary boundary are replayed chronologically as complete `user`/`assistant` message pairs when budget permits, with a compact `Tools used:` annotation on the assistant message when applicable
- Successful MCP tools from the latest four delivered exchanges in the active generation are pre-exposed on the initial model call only when they remain available to the current canonical user; this continuity query is independent of the latest summary boundary
- Each stored turn has an optional `expires_at`, but delivered transcripts and summary sources normally remain retained while their matching session generation is active; startup and periodic maintenance at `MEMORY_MAINTENANCE_INTERVAL` remove expired artifacts
- `/reset` advances the generation, deletes that tenant session's turns, summaries, and compaction jobs, and binds the latest tenant profile; the old transcript is no longer searchable
- Session expiry causes the next request to use a new generation, while cleanup removes inactive summaries, compaction jobs, turns, and the expired session. Generation counters are preserved so reset or expired generations are never reused
- `sessions` is the sole physical profile/session bookkeeping table: one row is retained per canonical user/session, including inactive rows, so generation high-water is never reused
- Each session row stores active/expiry state and its frozen profile version, renderer, digest, speaker intro, rendered snapshot, size/count metadata, profile high-water, and exact source memory IDs as a checked JSON array
- Cleanup deletes expired generation artifacts and marks the session inactive without deleting its row; memory deletion recompiles and rebinds snapshots whose JSON source membership contains the removed memory
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
- Startup always attempts a public OpenRouter model-catalog request before applying overrides and fallbacks. Catalog failure is logged as degraded and does not stop startup; deployments that block outbound access should set `MODEL_CONTEXT_WINDOW` and `MODEL_MAX_OUTPUT_TOKENS` explicitly for accurate budgeting

The prompt budget is the context window minus reserves for:

- response generation
- tool overhead
- safety margin

## Gateways

Gateway bootstrap is in `internal/gateway/bootstrap.go`.

- Home Assistant is enabled only when `HOME_ASSISTANT_AUTH_TOKEN` and `HOME_ASSISTANT_LISTEN_PORT` are both present and valid
- Discord is enabled only when `DISCORD_TOKEN` is non-empty after trimming
- iMessage is enabled only when `BLUEBUBBLES_URL`, `BLUEBUBBLES_PASSWORD`, and `BLUEBUBBLES_LISTEN_PORT` are all present and valid
- Incomplete or invalid configuration disables only that gateway; startup fails when zero gateways are configured correctly

### Home Assistant Gateway

Files:

- `internal/gateway/homeassistant/gateway.go`
- `internal/gateway/homeassistant/types.go`
- `internal/gateway/homeassistant/responder.go`

Behavior:

- Listens on `/homeassistant/ws` at `HOME_ASSISTANT_LISTEN_PORT`
- Requires exactly one bearer token matching `HOME_ASSISTANT_AUTH_TOKEN`; the configured token is hashed at startup and compared in constant time
- Sends `{"type":"ready","protocol_version":1}` immediately after a successful upgrade
- Accepts exactly one strict JSON conversation request per connection and rejects binary frames, unknown fields, anonymous users, missing conversation IDs, and empty text
- Trusts the authenticated Home Assistant service to assert a required HA user ID, then resolves or creates `gateway = 'homeassistant'` ownership for that ID
- Home Assistant principals use `home_assistant_token` assurance
- Session keys are `homeassistant:<ha-user-id>:<conversation-id>`; different users and conversations remain isolated
- Emits request-correlated `thinking`, `content`, `tool_call`, and `tool_result` frames followed by exactly one `result` or `error` frame
- Closes the socket normally after the terminal frame; account invalidation can close an active matching user connection
- Supports text requests only; images and command attachments are not supported
- `/bootstrap <code>` may be claimed by an authenticated Home Assistant user

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

- Listens for BlueBubbles webhook events at the fixed `/bluebubbles/webhook` path on `BLUEBUBBLES_LISTEN_PORT`
- Ignores self-authored messages and payloads with neither text nor attachments
- Normalizes iMessage handles into canonical phone-number or email identifiers
- Resolves account links using contact display names when available, with identifier fallback
- Incoming webhooks must present the configured BlueBubbles password through the `password` or `guid` query parameter, or the `x-password`, `x-guid`, or `x-bluebubbles-guid` header, before receiving `bluebubbles_webhook` assurance
- In direct chats, responds to all messages; in group chats, ordinary messages require `@oswald` or a reply to Oswald, while slash-command attempts require an Oswald mention
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
- `user_memory_search` — run deeper tenant-scoped hybrid retrieval with confidence and provenance
- `user_memory_list` — inspect active stored user facts
- `session_transcript_search` — search delivered role-preserving exchanges in the authenticated current session's active generation for exact episodic details
- `global_memory_search` — search administrator-curated facts about Oswald for the authenticated tenant
An untrusted compacted summary, recent completed exchanges, and bounded query-relevant durable user recall are injected automatically. Global memory is not automatically injected; the model calls `global_memory_search` for Oswald implementation, hardware, deployment, version, architecture, configuration, capability, and similar questions. Exact older session details remain available through `session_transcript_search`; deeper durable user retrieval remains model-directed through `user_memory_search` and `user_memory_list`. User-memory mutation is not exposed to the primary model.
Current time is not injected into the system prompt; the model must call `time.current` when an answer depends on it.

Optional external tools:

- MCP server configurations are stored in SQLite with plaintext model-visible descriptions and encrypted URLs and headers
- MCP server configuration is optional, but the subsystem is initialized unconditionally and `MCP_CONFIG_ENCRYPTION_KEY` is required at startup
- MCP-discovered tools are not included in the default LLM tool list; each described server exposes `<server>.tools` using exactly its configured description, that tool exposes returned matches for the current request, and eligible successful recent tools may be pre-exposed for continuity with their current live catalog descriptions
- `/mcp add <name> <https-url> [auth/header options] <description>` and its global form require a 1-500 character description; descriptions reject control characters and can contain unquoted spaces. The store permits empty descriptions only as the `v4.0.1` migration default for existing rows, and those rows expose neither discovery nor historically pre-exposed MCP tools until updated by rerunning the complete add command
- Global MCP servers are visible to all users; user MCP servers are visible only to their owning canonical user

### Tool Registry

The registry:

- loads markdown specs from disk
- converts them into LLM tool schemas
- maps tool names to handlers
- associates every handler with a validated runtime governance policy
- executes handlers when the model issues tool calls
- keeps builtin tools and MCP-discovered tools in the same runtime catalog

Runtime governance lives in `internal/tools/governance/`. Builtin policies are declared during registration; MCP discovery and remote tools use shared MCP-class policies. A request-local governor tracks attempts, actual executions, productive and unproductive results, failures, duplicates, and blocked calls. Exact duplicate fingerprints are hashed and never logged or persisted. Successful and unproductive executions retain their fingerprint, while execution failures release it so the model can retry the exact call. A zero per-tool limit disables that guard. Tools that exhaust an enabled per-tool allowance are hidden for the remainder of the request while unrelated tools remain available.

### MCP Integration

- MCP client startup lives in `internal/mcp/`; remote connections are opened lazily by discovery, testing, historical pre-exposure, or execution
- Server URLs must use HTTPS and pass public-address validation. Although `sse` exists as a stored transport value, only `streamable_http` is implemented; SSE-only configurations fail when connection is attempted
- Authentication is supplied through encrypted configured headers, including optional bearer headers
- Every valid remotely listed tool except the reserved remote name `tools` may be exposed and executed; there is no read-only mutation filter, so configured servers and their catalogs must be trusted. Remote tool and parameter descriptions are normalized, bounded, and included as model-visible schema metadata; safe bounded string enums are included without altering their exact values. Tool results remain explicitly untrusted
- MCP tools use namespaced names like `<server>.<tool>` and are surfaced through request-local discovery or eligible recent-tool pre-exposure; the `soul` server namespace is reserved and never model-visible

### Tool Governance

- Tool execution errors are converted into tool-response messages so the model can recover
- Successful handlers return a typed productive or unproductive outcome. `web.search` retires after two unproductive results in one request; other builtin and MCP tools have no per-tool execution, failure, or unproductive-result limit
- Exact duplicate successful or unproductive calls are blocked before handler execution, but failed executions release their fingerprint so the exact call can be retried
- Per-tool execution, failure, and unproductive limits are code-owned policy; zero disables a guard, and exhaustion of an enabled guard removes only that exact tool
- `MAX_TOOL_CALLS_PER_REQUEST` defaults to `50` actual handler executions as an emergency request-wide ceiling
- `MAX_TOOL_ITERATIONS_PER_REQUEST` defaults to `30` model responses containing tool calls as an emergency request-wide ceiling
- `MAX_TOOL_FAILURE_RETRIES` defaults to `0`, disabling the request-wide consecutive-failure guard; positive values re-enable it, and productive execution resets the streak
- Global limit exhaustion completes every declared call in the active batch with a correlated result, then asks the model to finish in one final call with all tools disabled
- Calls not advertised in the active model request are blocked. MCP discovery therefore exposes matching remote tools only for a subsequent model iteration, not later in the same emitted batch
- Broker lifecycle cancellation, tool-governance ceilings, transport failures, and the independently configured Bifrost provider timeout are the final safeguards; Oswald imposes no generation-duration deadline

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
- Streaming chat uses synchronous `/v1/chat/completions`; it is never routed through an async endpoint
- Non-streaming chat and tool-calling iterations use `POST /v1/async/chat/completions` followed by authenticated polling of `GET /v1/async/chat/completions/<job-id>` until Bifrost reports completion or failure
- Embeddings use `POST /v1/async/embeddings` followed by authenticated polling when `LLM_GATEWAY_EMBEDDING_MODEL` is set
- Bifrost async routes require a configured Logs Store. Bifrost's provider timeout is configured independently and remains responsible for bounding remote inference; Oswald has no LLM generation timeout
- Async job IDs are process-local and are not persisted. Cancellation or restart after submission may leave a remote job running until Bifrost completes or times it out; background retry ceilings bound repeated local submissions
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
- Discord and iMessage validate attachment metadata, enforce size limits, then validate the downloaded bytes using HTTP `Content-Type`, content sniffing, and HEIC/HEIF signature detection
- Decoded images are re-encoded as PNG when transparency must be preserved; otherwise they are re-encoded as JPEG
- Animated GIFs are sampled into one normalized contact-sheet image. Discord `gifv` embeds are converted from short video into a four-frame contact sheet using external `ffmpeg` and `ffprobe`, with static preview fallback when extraction fails
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

Production logging uses Loki-ready structured single-line JSON. The repository does not yet ship dashboards or additional periodic memory-quality aggregate emitters; those release-observability assets are deferred.

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
- durable state or security mutations, such as account linking, canonical user creation, admin changes, bans, and unbans

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
- provider response bodies or provider-supplied error messages, which may reflect prompt content
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

Current startup requirements:

- `LLM_GATEWAY_MODEL` must be non-empty
- `HOME_ASSISTANT_AUTH_TOKEN` and `HOME_ASSISTANT_LISTEN_PORT` must both be configured to enable Home Assistant; the token must contain at least 32 characters and the port must be an integer from 1 through 65535
- `BLUEBUBBLES_URL`, `BLUEBUBBLES_PASSWORD`, and `BLUEBUBBLES_LISTEN_PORT` must all be configured to enable iMessage; the URL must be absolute HTTP(S), the password non-empty, and the port an integer from 1 through 65535
- `DISCORD_TOKEN` must be non-empty to enable Discord
- Missing or invalid settings disable the affected gateway without failing immediately, but application startup fails if no gateway is configured correctly
- `MCP_CONFIG_ENCRYPTION_KEY` is required because the MCP store is initialized unconditionally, even when no server is configured
- `LLM_GATEWAY_URL` defaults to `http://localhost:8080`; API and virtual keys are optional
- The configured Bifrost gateway must enable a Logs Store for async chat and embedding routes and should use a finite provider timeout suitable for the deployment's slowest model

## Key Files

| File                                           | Purpose                                      |
| ---------------------------------------------- | -------------------------------------------- |
| `cmd/agent/main.go`                            | Startup wiring and shutdown                  |
| `internal/agent/agent.go`                      | Main agent loop                              |
| `internal/broker/broker.go`                    | Request queue and worker pool                |
| `internal/promptbudget/`                       | Context budget and prompt token estimates    |
| `internal/memoryformation/`                    | Pure memory evidence and activation policy   |
| `internal/formationruntime/`                   | Durable post-delivery memory extraction      |
| `internal/sessionruntime/`                     | Durable background session compaction worker |
| `internal/indexruntime/`                       | Derived-index lifecycle worker               |
| `internal/maintenanceruntime/`                 | Retention and SQLite maintenance worker      |
| `internal/runtimeinvalidation/`                | Runtime authorization/cache invalidation     |
| `internal/mcp/manager.go`                      | MCP client bootstrap and catalog             |
| `internal/routing/routing.go`                  | Shared gateway routing policy                |
| `internal/routing/types.go`                    | Gateway-neutral routing types                |
| `internal/llm/gateway.go`                      | LLM gateway HTTP client                      |
| `internal/modelinfo/`                          | Model metadata discovery                     |
| `internal/database/`                           | SQLite schema and database helpers           |
| `internal/tools/registry/`                     | Tool schema loading and execution            |
| `internal/tools/governance/`                   | Request-local tool policy and limits         |
| `internal/tools/runtime/`                      | Request-local tool exposure state            |
| `internal/tools/bootstrap.go`                  | Tool registry assembly                       |
| `internal/tools/builtin/`                      | Builtin tool wiring and handlers             |
| `internal/tools/builtin/globalmemory/`         | Shared global-memory store and handler        |
| `internal/tools/builtin/usermemory/store.go`   | Persistent per-user memory store             |
| `internal/soul/store.go`                       | Read-only soul system-prompt loader          |
| `internal/commands/service.go`                 | Shared command service                       |
| `internal/commands/parser.go`                  | Slash-command parser                         |
| `internal/commands/bootstrap/`                 | Process-local first-administrator bootstrap  |
| `internal/commands/accountlinking/store.go`    | Canonical account link store                 |
| `internal/commands/usermanagement/commands.go` | Admin and ban command handlers               |
| `internal/identity/principal.go`                | Typed request principal and assurance        |
| `internal/requestctx/requestctx.go`            | Request metadata propagation through context |
| `internal/media/images.go`                     | Image normalization and validation           |
| `internal/media/video.go`                      | Discord GIFV contact-sheet extraction        |
| `internal/gateway/runtime/`                    | Shared gateway request execution             |
| `internal/gateway/bootstrap.go`                | Gateway bootstrap                            |
| `internal/gateway/homeassistant/`              | Authenticated Home Assistant transport       |
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

### Adding A Migration

Never edit a released migration. Add exactly one `internal/database/migrations/vMAJOR.MINOR.PATCH.sql` file containing that release's direct SQL and extend fresh, prefix-ledger, checksum-drift, rollback, foreign-key, concurrency, and reopen coverage. Temporary pre-v4 import compatibility belongs under `internal/database/legacy_migrations/` and must not create pre-v4 ledger rows.

### Changing Personality

- Edit `data/memory/soul/soul.md` directly through operator filesystem or deployment access.
- Soul content is not exposed through model tools and cannot be changed by the agent.

Changes apply on the next request because the soul file is read fresh each time.

## Known Limitations

- Session summaries are model-generated continuity aids and may omit or misstate details; they are untrusted, and exact delivered details are available only while the active generation's transcript is retained
- `session_transcript_search` is lexical FTS5 search limited to the authenticated current session's active generation; it does not search reset, expired, or other sessions
- The gateway interface has no graceful stop method; gateway listeners remain active until process exit after worker shutdown begins
- Bifrost async job IDs are not persisted and async chat jobs have no documented cancellation endpoint, so process restart or post-submission cancellation may leave remote work running until Bifrost's independently configured provider timeout
- The MCP encryption key is required at startup even when no server is configured; MCP tools are not read-only filtered, and only public HTTPS streamable-HTTP endpoints are usable
- Formation startup reconciliation only recreates missing jobs for eligible turns from the previous 24 hours
- Only six builtin model tools ship locally; `global_memory_search` is the sole model-visible global-memory operation, and additional tools require MCP discovery or eligible recent-tool pre-exposure
- Application hard deletion cannot remove copies already retained by external database backups or log sinks; operators must configure those systems' retention separately
- While a replacement vector revision builds, semantic recall uses the old live revision and its embedding model; that old model must remain provider-accessible until replacement publication
- After the first successful embedding-dimension probe, the dimension is cached for the process lifetime; gateway-side route or dimension changes are not recognized until restart
- Discord GIFV contact sheets require external `ffmpeg` and `ffprobe`; general files, audio, and video attachments remain unsupported

Account-linking note:

- `data/database/oswald.db` stores canonical users and linked external accounts
- iMessage account records use normalized phone numbers or email addresses as the stable `identifier`
- iMessage `display_name` prefers a BlueBubbles-provided contact display name and falls back to the identifier when none is available
