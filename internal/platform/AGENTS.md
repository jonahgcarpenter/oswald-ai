# Platform - Config, State, Identity, Scheduling, Contracts

`internal/platform/` groups narrow infrastructure contracts. Each leaf owns its behavior; `platform`
itself is only the grouping, never a facade.

## Layout

- `config/` — strict YAML, private dotenv environments, interpolation, retention, safe structured
  logging (envelope, fields, error codes, ordering, activity catalog, log-contract test).
- `database/` — exact approved-schema `state.db` handle (`OpenState`, `ProfileSchemaVersion=1`,
  `schema.sql`). Foreign keys, secure delete, WAL, 5s busy timeout, single connection.
- `profiles/` — trusted transport-to-profile routing; `identity/` — principal/resolver contracts.
  `CanonicalUserID` carries the profile name; `user_id` is an untrusted display label.
- `approvals/` — in-memory single-flight pending approvals, five-minute window, no disk.
- `cron/` — per-profile jobs (`cron/jobs.json` + `executions.jsonl`), ticker, dispatcher.
- `requestctx/` — authenticated ownership and request-local metadata. `lease/` — durable
  heartbeat. `prune/` — creation-age sweeps and the shared hourly cleanup worker.

## Rules

- Config: 256 KiB cap, no symlinks/aliases/duplicate/unknown keys, depth ≤32. Interpolation is
  compose-style only (`${VAR}`, `:-`/`-` defaults, `:?`/`?` errors, `$$`); bare missing variables
  fail closed. Disabled platforms prune before interpolation but still reject unknown keys.
  Process env wins over dotenv; loading never mutates the process environment. Profile names are
  bounded lowercase ASCII; `default` reserved. `banned_users` always reject first.
- Database: initialize and verify only — no migrations, no account APIs. Fail closed on
  version/drift/foreign-key/unsafe-path violations. Maintenance runs immediately then hourly
  (≤100 expired sessions and ≤100 timed-out exchanges per transaction, 24h inactivity, 15-minute
  pending timeout, passive WAL checkpoint).
- Cron: named jobs with fuzzy id/name resolution; `scheduled|paused|completed|running` states;
  duration/`every`/5-field-cron/RFC3339/`in` schedules; injection/workdir/toolset validation;
  `ClaimDue` advances before dispatch with exact-instant ledger dedupe and half-period catch-up
  clamp (120s–2h, one-shot grace retire). Per-profile 30s ticker, 30m dispatch ceiling, origin-only
  delivery (`[SILENT]` suppresses, nothing mirrors into history).
- Logging: single-line JSON to stderr, universal envelope, correlation-first details, fixed events
  and literal keys, numeric counts, safe bounded labels. INFO is the compact lifecycle stream;
  WARN is recoverable degradation; ERROR is startup/stopped-gateway/safety failures. Never log
  values or raw errors; the AST guard is necessary but not sufficient — review sources.

## Testing

Cover interpolation success/failure codes, fresh/reopen/drift/rollback/concurrency, routing and
admission, cron claim/dedupe/catch-up, approval windows, lease renewal, prune TTLs, and log
emission counts, levels, and correlation. Use the log-contract test as the gate, not the proof.
