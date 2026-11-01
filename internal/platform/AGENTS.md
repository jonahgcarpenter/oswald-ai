# platform/ — Config, State Handles, Routing, Scheduling, Logging

Applies on top of the root `AGENTS.md` (operator-state, persisted-contract, and never-log
invariants). This directory groups narrow contracts; it is never a facade — behavior lives in the
leaf packages.

## Config (`config/`)

Strict YAML with private per-profile dotenv environments. Interpolation is compose-style only —
`${VAR}`, `${VAR:-default}`, `${VAR-default}`, `${VAR:?err}`, `$$` — and bare missing variables
fail closed with a classified `config_*` diagnostic naming only the schema path, artifact, and
variable (values and error text are never logged). Disabled platforms prune before interpolation
so their credentials are never required, but unknown keys inside a disabled block are still
rejected. Process env wins over dotenv and loading never mutates the process environment; named
profiles inherit the default `.env` and override per key, and can only add providers, model, tools,
MCP servers, and `api` — never shared listeners or routes.

Files are bounded (256 KiB, no symlinks/aliases/duplicate/unknown keys, depth ≤32) and dotenv
files must be private regular files. Profile names are bounded lowercase ASCII with `default`
reserved. The supported surface is defined by what `oswald setup` writes; former
application-setting environment variables are not a deployment interface.

All CLI mutation shares one mediated path (`CommitConfigCandidate`: staged startup `Validate`
of the candidate in a temp root before anything touches disk, previous bytes kept as the
last-known-good sibling, owner-only atomic replace). Failures leave disk untouched and report
classified `config_*` diagnostics naming only the schema path, artifact, and variable — never
values. `oswald config` itself is read-only (`show|get|validate`); each top-level `config.yaml`
section has a scoped command group (`providers`, `model`, `api`, `gateway`, `platforms`,
`tools`, `runtime`, `mcp_servers` via `oswald mcp`) that whitelists its own leaves, so no edit
can address — or break — a sibling section. `ClassifyReload` diffs effective configs by section
so each command reports `applied live` (runtime log level, admission allow/ban/mention rules)
vs `restarting service to apply` (everything else, picked up by the server's supervised
rebuild within seconds). Scope is fenced at the same layer: `ResolveReadProfile` opens reads
to any existing profile, `ResolveProfile` keeps mutations on the calling scope with
contradictions failing closed, and `RequireDefaultScope` gates global mutations and profile
lifecycle to the default scope.

## State handles (`database/`)

`OpenState` initializes an empty profile `state.db` transactionally with one version-1
`schema_version` row and verifies by comparing every schema object (generated ones included)
against the `schema.sql` baseline with formatting-insensitive token normalization. Existing
incompatible, drifted, or foreign-key-violating databases fail closed — there are no migrations,
no account APIs, and no upgrade path. DSN pins foreign keys, secure delete, WAL with synchronous
NORMAL, a 5s busy timeout, immediate transactions, and a 1,000-page checkpoint; one connection max.
Maintenance runs immediately then hourly: at most 100 expired sessions and 100 timed-out pending
exchanges per transaction, 24h inactivity, 15-minute pending timeout, passive WAL checkpoint.

## Routing and identity (`profiles/`, `identity/`, `approvals/`)

`profiles/` maps transport-authenticated identities to existing profiles (`bluebubbles`
normalizes to internal `imessage`), falling back to the default profile when no route matches;
explicit default routes need no multiplexing, named ones do. `banned_users` always reject first;
empty `allowed_users` admits every routed identity — and admission runs before attachment or reply
work. `identity/` defines the principal contract: `CanonicalUserID` carries the trusted profile
name while `user_id` stays an untrusted display label, never ownership; the `cron` gateway's
`scheduler` assurance marks profile-owned dispatch without passing transport routing.
`approvals/` holds single-flight pending terminal approvals in memory only (five-minute window;
expiry, restart, `/new`, and `/stop` all deny).

## Scheduling (`cron/`)

Per-profile jobs in `cron/jobs.json` with atomic rewrites (plus `executions.jsonl`, pruned to the
newest 1000): `scheduled|paused|completed|running` states plus `enabled`, fuzzy id-or-name
resolution, concrete `deliver{platform,chat_id,user_id}` targets (no origin indirection; groupchat
`user_id` mentions, DMs ignore it), `repeat{times,null=forever,completed}` auto-complete, and
`failure_streak` auto-pause at 3 with a target alert. Schedules store the parsed dict
`{kind:cron|interval|once,expr|minutes|run_at,display}` with UTC ISO `created_at/next_run_at`
(`null` when unscheduled); the grammar covers durations, `every` phrases, 5-field cron, RFC3339,
and `in` one-shots, all UTC wall-clock. Scripts live under per-profile `scripts/` (jailed,
must exist). Validation rejects injection, script escapes, bad deliver targets, and user-owned
`model/provider/base_url`; unknown or legacy fields fail closed with a manual-update error (no
migration). `ClaimDue` advances `next_run_at` before dispatch with exact-instant ledger dedupe
(skipped duplicates stay silent), restore-once for orphaned runs, and one-shot grace retire;
unparsable schedules record `blocked_config` instead of running. A per-profile 30s ticker with
re-arm wake drives dispatch at a 30m ceiling — `[SILENT]` suppresses, and nothing mirrors into
history.

## Shared contracts (`requestctx/`, `lease/`, `prune/`)

`requestctx` carries authenticated ownership and request-local metadata (tool exposure, input
images) without fabricating unavailable transport fields. `lease` renews a durable exact-token
lease at a third of its TTL while blocking work runs, and renewal failure cancels the work.
`prune` owns creation-time age (statx birthtime, mtime fallback) and declarative directory/TTL
sweeps through the single per-profile cleanup worker that owner hooks (images, scratch, process,
unknown `cache/` children) register with.

## Logging (read this before touching any log call)

Production stderr is single-line JSON with one universal envelope
`ts, level, event, component, log_type, details`, details last with `msg` first, then correlation
(`profile, user_id, gateway, request_id, operation/job, model`), then alphabetically sorted fields,
`error_code` last. Success carries no `status` field; only error/rejected/retry/degraded are
emitted. String labels ≤256 bytes, messages ≤1024, records ≤16 KiB, scalar lists ≤16 — oversize or
unsupported values produce marshal-failure diagnostics, not truncated secrets. The field
classification is allowlist-based: unknown or private-suffixed keys are omitted, and output
filtering is not a secret detector, so source-value review stays mandatory.

INFO is exactly eight lifecycle events (`app.started/stopped`, `gateway.connected`,
`chat.requested/completed`, `command.completed`, `tool.completed/blocked`); everything else is DEBUG telemetry, WARN for recoverable
degradation, ERROR for startup failures and stopped gateways. Correlate by `request_id`; never add
requested/completed or chat/tool/diagnostic counts together — a missing completion may mean still
running. `app.started` means composition finished, not listener readiness. Use
`log.Server(component)` for infrastructure and `log.Agent(...)` for request work, and propagate
`requestctx.LogFields` for correlation.

## Tests

Cover interpolation success/failure codes, fresh/reopen/drift/rollback/concurrency on state,
routing and admission, cron claim/dedupe/catch-up, approval windows, lease renewal, prune TTLs —
and log emission counts, levels, scalar types, and correlation. The logging AST guard checks shape,
not values; the tests are the gate, source review is the proof.
