# app/ — Composition Root, CLI Dispatch, Chat Commands

Applies on top of the root `AGENTS.md` (main-only-exit and operator-state rules).

## Shape

`startup/serve.go::Serve` is the sole entry: load profiles → print the TTY-gated banner → install
signal handling → `Run`, logging a classified failure on the way out. `startup/profiles.go`
assembles everything per profile in sorted order (state store, maintenance, file/soul stores,
builtin registry, model client, markdown summarizer, profile MCP manager, agent,
image cache, shared prune worker with its image/scratch/process/unknown-children hooks), then the
shared foreground broker, profile commands (including `/compact`), delivery markers, and the
configured gateways with profile-selected dependencies. Shutdown runs the same list in reverse —
outbound, prune, maintenance, broker, cron ticker, `process.StopAll`, MCP sessions, databases —
with workers joined before stores close, and errors returned only after cleanup. Nothing in
`main` or `cli` assembles stores, workers, or gateways; gateway start failures are asynchronous
logs, and listeners can outlive `Run` until process exit.

`cli/cli.go` dispatches `setup | start | profile | mcp | gateway | config | env | help` with exit codes 0/1/2 and injectable
stdio. `setup` seeds missing entries only and rolls back partial state; `profile create` scaffolds
one private subtree (`.env`, minimal `config.yaml`, approved-schema `state.db`, root `SOUL.md`
copy) and `profile delete` refuses `default` over an explicit `y`/`yes`. `mcp auth` is the only
interactive OAuth path. `gateway <name> <enable|disable>` flips only the global
`platforms.<name>.enabled` flag (`discord|bluebubbles|api`, exact match, no aliases) via an
atomic `config.yaml` rewrite; the running server picks it up on restart. `commands/` holds the `Definition/Request/Result/Handler` contract and
`/name` parsing; `builtin/` registers the six public handlers (`/help`, `/new`, `/stop`,
`/approve`, `/deny`, `/compact`).

## Behaviors worth knowing cold

- `/new` ends the open session (`end_reason='new_session'`), bumps generation, and denies pending
  approvals — history stays searchable under the old generation.
- `/stop` cancels active agent work in the conversation and preserves queued prompts; `/stop all`
  is rejected. Commands outlive transport cancellation.
- `/approve` replays the identical terminal invocation through lifecycle, self-repo, sandbox, and
  the unrecoverable/fetch/config re-checks (destructive detection skipped by user override);
  anything else — expiry past five minutes, restart, `/new`, `/stop`, the API — denies.
- `/compact` forces markdown summarization of all but the newest 3 turns in-lane, flags
  them `compacted=1`, and caches the summary; the result applies to the next reply
  (no-op at ≤3 turns). Auto compaction fires at 70% input pressure the same way.
- Server binaries never import the `cli` tree.

## Tests

Inject clients and gateways rather than starting real listeners. Cover dispatch exit codes,
seed-only setup, create/delete guards and rollback, command aliasing, approval consumption, and
partial-failure shutdown order.
