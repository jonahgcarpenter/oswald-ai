# Platform - Config, State, Identity, Scheduling, Contracts

`internal/platform/` groups narrow infrastructure contracts. Each leaf owns its behavior; `platform`
itself is only the grouping, never a facade.

## Packages

- `config` (`config/`) — strict YAML loading, profile dotenv environments and interpolation,
  document merging, retention policy, and the safe logging system (envelope, fields, error codes,
  ordering, activity catalog, contract test).
- `database` (`database/`) — exact approved-schema `state.db` handle: `OpenState` verification,
  `schema.sql` baseline, single-connection ownership. No migrations.
- `profiles` (`profiles/`) — immutable transport-to-profile routing table: identity normalization,
  allow/ban admission, route resolution with default fallback.
- `identity` (`identity/`) — `Principal`/`Resolver` contracts: `CanonicalUserID` profile binding,
  gateway assurances. Never provisions accounts.
- `approvals` (`approvals/`) — in-memory single-flight pending terminal approvals, five-minute
  window, no disk.
- `cron` (`cron/`) — per-profile schedules, atomic `jobs.json` store, 30s ticker, dispatcher with
  origin-only delivery.
- `requestctx` (`requestctx/`) — authenticated ownership and request-local metadata plus telemetry
  field helpers.
- `lease` (`lease/`) — durable heartbeat that renews a lease while blocking work runs.
- `prune` (`prune/`) — creation-age specs and the single shared hourly cleanup worker that owner
  hooks register with.

## Rules

- Config files: 256 KiB cap, no symlinks/aliases/duplicate/unknown keys, depth ≤32. Interpolation
  is compose-style only (`${VAR}`, `:-`/`-` defaults, `:?`/`?` errors, `$$`); bare missing variables
  fail closed. Disabled platforms prune before interpolation but still reject unknown keys. Process
  env wins over dotenv; loading never mutates the process environment. Profile names are bounded
  lowercase ASCII; `default` reserved. `banned_users` always reject first.
- Database: initialize and verify only. Fail closed on version/drift/foreign-key/unsafe-path
  violations. Maintenance runs immediately then hourly (≤100 expired sessions and ≤100 timed-out
  exchanges per transaction, 24h inactivity, 15-minute pending timeout, passive WAL checkpoint).
- Cron: named jobs with fuzzy id/name resolution; `scheduled|paused|completed|running` states;
  duration/`every`/5-field-cron/RFC3339/`in` schedules; injection/workdir/toolset validation;
  `ClaimDue` advances before dispatch with exact-instant ledger dedupe and half-period catch-up
  clamp (120s–2h, one-shot grace retire). Per-profile 30s ticker, 30m dispatch ceiling, origin-only
  delivery (`[SILENT]` suppresses, nothing mirrors into history).
- Logging: single-line JSON to stderr with the universal envelope, correlation-first details, fixed
  events and literal keys, numeric counts, safe bounded labels. INFO is the compact lifecycle
  stream; WARN is recoverable degradation; ERROR is startup/stopped-gateway/safety failures. The
  AST contract test gates emission shape — it does not prove values are safe; review sources.

## Testing

Cover interpolation success/failure codes, fresh/reopen/drift/rollback/concurrency, routing and
admission, cron claim/dedupe/catch-up, approval windows, lease renewal, prune TTLs, and log
emission counts, levels, and correlation.
