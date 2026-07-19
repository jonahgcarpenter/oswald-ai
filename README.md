# Oswald AI - Uncensored Digital Servant

> Fully local, fully uncensored, zero costly API dependencies.

## Overview

Oswald AI is a local-first Go application built around a single agentic chat loop.
The model receives the user prompt, can call registered tools, and then returns a final streamed response in Oswald's voice.

## Features

- Iterative tool-calling agent loop on top of an OpenAI-compatible LLM gateway
- iMessage, Discord, and WebSocket gateway
- HACS integration for Home Assistant [here](https://github.com/jonahgcarpenter/has-oswald-conversation)
- Nine builtin tools: `web.search`, `time.current`, `memory.save`, `memory.search`, `memory.list`, `memory.forget`, `transcript.search`, `soul.read`, and `soul.patch`
- Traceable durable-memory formation with grounded candidates, conservative automatic policy, conversational confirmation for sensitive facts, atomic publication, and recoverable post-turn extraction
- User & Global MCP integrations
- SQLite-backed session chat memory with proactive durable background compaction, untrusted structured summaries, a recent role-correct tail, active-generation transcript search, and TTL expiry
- Tenant-scoped hybrid durable-memory recall using FTS5 and sqlite-vec
- Authenticated self-service privacy inspection, export, forgetting, deletion, and account erasure
- Rebuildable revisioned retrieval indexes, configurable retention, and privacy-safe memory-health telemetry
- FIFO execution per user/session, with parallel processing across independent conversations
- Per-user persistent memory in SQLite and a live-editable soul file
- Fully local runtime with no cloud hosted model dependency

## Memory

Oswald uses three memory layers:

- Soul memory: `data/memory/soul/soul.md`, read fresh on every request
- Persistent user memory: `data/database/oswald.db`, managed by `memory.*` tools
- Session chat memory: delivered, completed exchanges in SQLite `session_turns`, compacted in the background into immutable structured checkpoints with source-turn links while retaining a minimum recent verbatim tail

Long active conversations use an explicitly untrusted generated summary followed by recent complete `user`/`assistant` exchanges. Compaction runs serially from durable jobs only after a response is successfully delivered, keeps the covered transcript intact, and can stage source-grounded memory candidates through the normal approval policy without directly activating them.

`transcript.search` performs FTS5 search over delivered exchanges retained in the authenticated current session's active generation and returns role-preserving excerpts with session and turn provenance. It is for exact episodic history; `memory.search` remains the tool for durable user facts. `/reset` deletes the session's turns, summaries, and compaction jobs and advances its generation, while expiry cleanup removes inactive session artifacts; reset or expired transcripts are not searchable.

`AGENTS.md` documents the full runtime and architecture in detail.

Local Go builds and tests must enable SQLite FTS5, for example `go test -tags sqlite_fts5 ./...`.

### Storage and index safety

SQLite canonical rows are the source of truth. FTS5 and sqlite-vec tables are derived, revisioned state maintained from a durable outbox. The index worker builds replacement revisions as shadow tables, applies changes to both live and building revisions, validates exact canonical ownership and coverage, and atomically publishes only a complete revision. A failed build does not displace the old live revision, and lexical and semantic recall degrade independently.

Startup runs ordered schema migrations `v1` through `v11` in one `BEGIN IMMEDIATE` transaction, checks every recorded name and SHA-256 checksum for drift, and runs `PRAGMA foreign_key_check` before commit. Unknown versions, changed names/checksums, required migration failures, or foreign-key violations stop startup. The two FTS5 baselines (`v5` and `v6`) are optional capabilities: an unavailable FTS5 capability is logged as degraded and its frozen migration is still recorded. Exact legacy symbolic checksums for `v1` through `v6` are accepted once, then rebased to checksums of the frozen definitions.

### Privacy and retention

Every `/privacy` operation requires an authenticated principal in a direct conversation. Inspection returns stable IDs and lifecycle metadata, not content. Exact-ID mutations accept only positive decimal IDs. Bulk memory deletion and account erasure issue one-time, actor-bound confirmation codes that expire after 10 minutes.

| Command | Effect |
| --- | --- |
| `/privacy inspect [memories\|candidates\|sessions\|all] [page]` | List bounded metadata-only records and stable IDs. |
| `/privacy export` | Export one consistent `oswald.user-export.v1` JSON snapshot. |
| `/privacy forget-memory <id>` | Remove a memory from profiles and retrieval immediately, then scrub canonical content after the default 30-day grace period. |
| `/privacy delete-memory <id>` | Immediately and irreversibly scrub one memory and its linked source exchange. |
| `/privacy delete-candidate <id>` | Scrub one candidate, any published memory, and linked source exchange. |
| `/privacy delete-session` | Delete the current session generation, summaries, jobs, turns, and derived transcript rows. |
| `/privacy delete-all-memories` | After confirmation, scrub all memories and candidates and their source exchanges while retaining the account. |
| `/privacy delete-account` | After confirmation, erase the canonical user, accounts, memory/session state, user MCP configuration, and derived rows. |
| `/privacy confirm <code>` | Consume an actor-bound pending bulk-operation code. |

Exports larger than 8 MiB are delivered in ordered `.partNNN` files; concatenate them byte-for-byte in filename order to reconstruct the JSON. The total export cap is 80 MiB. User MCP exports contain metadata only and redact the encrypted endpoint and credentials.

Privacy mutations fence the whole canonical user while committing, enqueue durable runtime invalidation in the same transaction, clear affected gateway/session caches, and close authenticated connections after account erasure. A later message from an erased external identity creates a new blank canonical user; erased data is not restored. Verified account merges preserve both tenants' memories, sessions, profile state, audit metadata, derived-index work, moderation references, admin status, linked accounts, and encrypted user MCP ownership under the challenge initiator's canonical user.

Ordinary group memory remains enabled by explicit product decision: an addressed group turn uses that authenticated sender's private memory and session scope. It never creates shared group memory. All `memory.*` and `transcript.search` handlers require an authenticated principal. The nine model tools listed above are unchanged; `/privacy` operations are gateway commands, not model tools.

Retention defaults are balanced for local operation and configurable with the `MEMORY_*` variables in `.env.example`. Maintenance runs immediately at startup and hourly thereafter in bounded batches, checks foreign keys before mutation, expires sessions, redacts old content-bearing audit/job records, removes due tombstones and orphan index rows, verifies exact index coverage, checkpoints WAL passively, uses incremental vacuum only when configured by SQLite, and runs `PRAGMA optimize` daily. SQLite is opened with `secure_delete=ON`, WAL, `synchronous=NORMAL`, and a 1000-page auto-checkpoint.

The application cannot erase copies already captured by external backups or an external log sink; those systems need their own retention policy. During a vector-model shadow rebuild, the old model must remain accessible through the configured provider because live semantic recall continues to use the old live revision until the replacement validates and publishes.

The privacy-safe Grafana/Loki dashboard is `monitoring/grafana/dashboards/oswald-memory-health.json`.

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
| `/privacy` | `/privacy inspect [memories\|candidates\|sessions\|all] [page] \| export \| forget-memory <id> \| delete-memory <id> \| delete-candidate <id> \| delete-session \| delete-all-memories \| delete-account \| confirm <code>` | Inspect, export, forget, or erase your retained data in an authenticated direct conversation. |
| `/mcp servers` | `/mcp servers` | List your user-scoped MCP servers. |
| `/mcp add` | `/mcp add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>]` | Add or update a user-scoped MCP server. URLs and headers are encrypted at rest. |
| `/mcp remove` | `/mcp remove <name>` | Remove one of your MCP servers. |
| `/mcp enable` | `/mcp enable <name>` | Enable one of your MCP servers. |
| `/mcp disable` | `/mcp disable <name>` | Disable one of your MCP servers. |
| `/mcp test` | `/mcp test <name>` | Connect to one of your MCP servers and report discovered tool count. |

Eligible durable identity and preference memories are compiled into a bounded, lower-authority tenant profile. A profile is frozen for each conversation session; changes become automatic context in new sessions or after `/reset`. Each current turn also receives a small, budgeted set of relevant tenant-scoped memories from hybrid lexical and semantic recall, while `memory.search` remains available for deeper investigation.

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
