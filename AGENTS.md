# Oswald Agent - Development Guide

For AI coding assistants and developers working on oswald-ai. This root file is a hub: what
applies everywhere, then a routing table. Each area's `AGENTS.md` loads automatically when you work
in that directory; read it before editing there. Source, the operator-approved
`internal/platform/database/schema.sql`, and tests define implemented behavior.

## What Oswald Is

Oswald is a personal AI agent service written in Go. It runs the same agent core across Discord,
iMessage (BlueBubbles), and an OpenAI-compatible HTTP API. It keeps per-profile sessions in SQLite,
learns persistent operator policy from `SOUL.md`/`USER.md`/`MEMORY.md`, runs MCP-bridged and builtin
tools, scheduled cron jobs, and tracked background shell processes.

Two invariants shape almost every design decision and are the lens for reviewing any change:

- **Operator state is sacred.** Config, credentials, souls, memory, caches, and `state.db` are never
  migrated, rebuilt, upgraded, or deleted by code. Tests use synthetic principals and temporary
  approved-schema databases/files, never live credentials or a runtime database. Anything that fails
  verification fails closed.
- **Persisted contracts are load-bearing.** The approved `schema.sql`, `state_meta` JSON shapes,
  generation high-water marks, external tool/command/route names, and log event names are not
  refactored casually. Internal Go package paths may move; wire formats, DDL, and user-facing names
  may not. Database changes require explicit operator approval, and approval never authorizes
  deleting an operator database.

## Contribution rubric

**Wanted:** real bug fixes with a repro on the current base and the exact line; small cohesive
changes grouped by responsibility; behaviour-contract tests.

**Rejected even when well-built:** generic utility packages and unnecessary service/interface layers;
compatibility APIs solely for obsolete fixtures; shims that hide a failed verification instead of
fixing it; change-detector or source-reading tests.

**Before you call it a bug,** check source and tests first: profiles are islands on purpose,
`/new` keeps history by design, delivery gating is intentional. A fix that cannot point to the line
where the behavior manifests has an unverified premise.

**Filing issues:** use `.github/ISSUE_TEMPLATE/bug_report.md` or `feature_request.md`
as the body shape. No Evidence section (repro + exact line), no fix — the issue goes
back for questions first.

**Security:** endpoints, catalogs, commands, and credentials are trusted operator configuration, not
a hardened boundary. Never log prompts, answers, reasoning, tool payloads, URLs, transport
identities, session keys, images, credentials, dotenv contents, or raw errors. Scope values are
never logged.

## Development Environment

This is a Go application, not a JavaScript/frontend project. Module declares Go 1.25.7. SQLite and
image codecs need CGO and a C/C++ toolchain; use the `sqlite_fts5` build tag on every command or
SQLite symbols fail. Discord GIFV extraction needs `ffmpeg` and `ffprobe`.

```bash
go build -tags sqlite_fts5 ./...
go test -tags sqlite_fts5 ./...
go test -race -tags sqlite_fts5 ./...
go vet -tags sqlite_fts5 ./...
git diff --check
```

Single package or test: `go test -tags sqlite_fts5 ./internal/broker/ -run TestName`. `air` at the
repo root rebuilds both binaries with the tag into `./tmp/` for the dev loop. `GOTOOLCHAIN=go1.25.7`
overrides a different installed toolchain.

Tests use synthetic principals, temporary approved-schema databases/files, fake model
clients/transports, local HTTP handlers, explicit clocks, and joined goroutines. They must not need
secrets or live services. The release workflow runs tagged tests before publishing its container; it
does not enforce every race/vet/formatting check on PRs.

## Project Structure

Counts shift; the filesystem is canonical. Load-bearing entry points:

```
oswald-ai/
├── cmd/oswald/            # operator CLI binary; delegates to internal/app/cli, owns process exit
├── cmd/oswald-server/     # service binary; delegates to internal/app/startup, owns process exit
├── internal/app/          # startup (composition root) + cli (dispatch) + commands (/help/new/stop/approve/deny)
├── internal/agent/        # iterative foreground loop: context, streaming, tools/history, images, retries
├── internal/broker/       # FIFO conversation lanes, profile fences, bounded admission
├── internal/session/      # memory (sessions/delivery/history) + compaction (+budget) + soul (policy)
├── internal/tools/        # builtin registry/governance/exposure + tool groups (memory, search, web, vision, image)
├── internal/workspace/    # workspace file/shell tools + process (tracked background processes)
├── internal/transport/    # gateway (discord, imessage, openai + routing/runtime) + media (normalize/cache)
├── internal/providers/    # llm (model contract) + web (brave/searxng) + image_generate (comfy_ui)
├── internal/mcp/          # profile-configured bridge (tool_search/describe/call), OAuth, projection
└── internal/platform/     # config + database (state.db) + profiles/identity/approvals + cron +
                           # requestctx/lease/prune (narrow shared contracts)
```

**User state:** `.oswald/` (default/global root: `config.yaml`, private `.env`, `SOUL.md`, `memories/`,
`cache/`, `state.db`); named profiles at `.oswald/profiles/<name>/`. `oswald setup` seeds missing
entries only and never overwrites; `oswald profile create/delete` provisions/removes one subtree.

### Responsibility-file layout

Each package keeps one topic per file (`agent/context.go`, `agent/tool_history.go`,
`session/memory/profile_search.go`, `transport/gateway/discord/inbound.go`). Find code by topic with
`grep`, not by reading a single god file. Shared names (`platform`, `session`, `transport`, `app`)
are directory groupings, not utility facades; behavior lives in the leaf package that owns it.
Moving a symbol means fixing its docs in the same change.

## Rules that apply everywhere

- **Go style:** `gofmt`, standard-library/third-party/internal import groups, exported API doc
  comments, context-first cancelable APIs, wrapped causes (`%w`), typed errors, explicit
  failure/state transitions.
- **Only `main` exits or logs fatal errors.** Domain/startup APIs return errors after cleanup.
  Preserve resource ownership, rollback, row-iteration checks, exact-token leases, delivery gates,
  and generation fences.
- **Never rename the outside world.** Preserve external tool names, JSON fields, commands, routes,
  environment references, log keys, and persisted formats during internal refactors.
- **Do not edit `README.md`** unless the user explicitly requests it. Update contributor docs here.
- **Branches:** short kebab-case summary only (e.g. `imessage-stop-typing`).
- **Commits:** `<type>: <change>` subject (e.g. `fix: stop typing indicator after each send`); one to
  two line body naming the area plus what changed.
- **Logging:** fixed developer-owned events/messages, literal reviewed field keys, numeric
  counts/units. `profile` is the canonical correlation key; `user_id` is an untrusted display label,
  never ownership. See `internal/platform/AGENTS.md` for the envelope and event catalog.

## Routing Table — working in X → read X/AGENTS.md

| Area | Read | Covers |
|---|---|---|
| `internal/app/`, `cmd/` | `internal/app/AGENTS.md` | composition root, startup/shutdown order, CLI dispatch, chat commands |
| `internal/agent/` | `internal/agent/AGENTS.md` | turn loop, system prompt, budget, tool authorization, retries, MEDIA delivery |
| `internal/broker/` | `internal/broker/AGENTS.md` | lanes, fences, admission, cancellation, foreground priority |
| `internal/session/` | `internal/session/AGENTS.md` | delivery gating, history, `state_meta`, `/new`, soul binding, compaction |
| `internal/tools/` | `internal/tools/AGENTS.md` | registry, governance, toolsets, builtin inventory, history bounds |
| `internal/vault/` | `internal/vault/AGENTS.md` | encrypted profile credential store, vault autofill scripts |
| `internal/workspace/` | `internal/workspace/AGENTS.md` | file/shell tools, pre-execution chain, reviewer, approvals, processes |
| `internal/transport/` | `internal/transport/AGENTS.md` | admission, mentions, replies, streaming, outbound, media normalization |
| `internal/providers/` | `internal/providers/AGENTS.md` | LLM contract, web backends, image generation graphs |
| `internal/mcp/` | `internal/mcp/AGENTS.md` | bridge protocol, projection, OAuth, trusted-operator boundary |
| `internal/platform/` | `internal/platform/AGENTS.md` | config, state handles, routing/identity, cron, shared contracts, logging |
