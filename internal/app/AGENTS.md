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

`cli/cli.go` dispatches `setup | start | profile | mcp | gateway | platforms |
providers | model | api | tools | runtime | config | env | help` with exit codes 0/1/2 and injectable
stdio. `setup` seeds missing entries only and rolls back partial state; `profile create` scaffolds
one private subtree (`.env`, minimal `config.yaml`, approved-schema `state.db`, root `SOUL.md`
copy) and `profile delete` refuses `default` over an explicit `y`/`yes`. `mcp auth` is the only
interactive OAuth path. `platforms <name> <enable|disable>` flips only the global
`platforms.<name>.enabled` flag (`discord|bluebubbles|api`, exact match, no aliases) through the
mediated write path (staged validation, last-known-good, atomic replace); `gateway` owns only
`gateway.multiplex_profiles + profile_routes`. `config` is read-only (`show|get|validate`).
Scoped groups mirror the top-level `config.yaml` keys one-to-one: `providers` → `providers.*`,
`model` → `model.*`, `api` → `api.key`, `gateway` → `gateway.*`, `platforms` → `platforms.*`,
`tools` → `tools.*`, `runtime` → `runtime.*`, `mcp` → `mcp_servers.*`. `--profile` is accepted
(in any position) only where profiles may override (`providers`, `model`, `api`, `tools`, `mcp`);
`gateway`, `platforms`, and `runtime` are global-only. Scoping is read-vs-mutate: reads
(`show`, `get`, `list`, `validate`, `test`) may name another existing profile, while every
mutation stays fenced to the calling scope (`OSWALD_HOME` profile) and fails closed on
contradiction. Global mutations (`gateway` writes, `platforms`, `runtime`) and profile
lifecycle (`profile create|rename|delete`) additionally require the default scope. `commands/` holds the `Definition/Request/Result/Handler` contract and
`/name` parsing; `builtin/` registers the six public handlers (`/help`, `/new`, `/stop`,
`/approve`, `/deny`, `/compact`).

## Validation and reload (how edits take effect)

Every mutating group funnels through `platform/config.CommitConfigCandidate`: the candidate is
staged through the exact startup `Validate` pipeline in a temp root, and only on success are the
previous bytes saved as last-known-good and the candidate atomically written. A rejected edit
leaves the file byte-identical and reports a classified `config_*` diagnostic. After a successful
write the command diffs pre/post effective configs with `config.ClassifyReload` and prints
`applied live` vs `restart required to take effect`.

The running server needs no signal: `startup/reload.go::Reloader` fingerprints every profile's
`config.yaml`/`.env` on a 2s poll (500ms settle), then on change validates the candidate —
invalid keeps the live config with a `config.reload.rejected` warning — and hot-swaps only the
`runtime` log level and gateway admission policy (`config.reload.applied`); a validated non-hot
diff requests a supervised rebuild instead. `runProfilesWith` returns `errRebuildNeeded` after
the existing reverse-order cleanup joins every worker, and `Serve` re-execs the binary
(`execSelf`, same args and environment) so listeners rebind cleanly. Log trail for a rebuild:
`config.reload.restart_required` (with `reason_code`) → `app.stopped`
(`cleanup_reason=rebuild`) → fresh boot `app.started`. The watcher never rewrites operator
files, and a stable fingerprint never fires twice, so rapid successive writes coalesce.

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
seed-only setup, create/delete guards and rollback, command aliasing, approval consumption,
partial-failure shutdown order, and rebuild-once signaling (validated non-hot fires once;
hot and invalid never fire).
