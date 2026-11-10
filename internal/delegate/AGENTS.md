# delegate/ — Scoped Subagent Delegation with Durable Delivery

Applies on top of the root `AGENTS.md` (operator-state and persisted-contract rules).

## Shape

One package. `delegate.go` holds the per-profile `Manager`, the `BlockedTools`
withholding list, and the `Spawn` depth/role carrier; `manager.go` is the
registry entry point (`Execute`: dispatch, inline nested runs, list/steer/stop);
`child.go` builds the child brief and runs one synchronous turn (output-schema
validate plus one bounded correction, transcript streaming, orphan reporting);
`worker.go` runs dispatched units through the pool with single-winner
completion (`finishUnit`); `control.go` drives live-unit actions and
`/new` cancellation; `delivery.go` formats terminal results as new-turn
prompts and submits them through the broker lane; `monitor.go` runs the stall
sweep with grace-then-force-finalize; `envelope.go` builds the durable dispatch
envelope; `lifecycle.go` resolves model/provider routing and owns Start/Stop.

## How it works

Top-level dispatches always run asynchronously: one ledger row per unit
(`async_delegations`, state running, delivery pending), an immediate handle
receipt, and results arriving later as new turns in the origin session.
Nested orchestrator calls run inline in the calling turn and never touch the
ledger. Children are stateless, `SkipMemory`, and keep the parent principal
unchanged; their catalog is the parent's toolsets minus `delegate` (unless
nesting is enabled and allowed), `clarify`, `memory`, `session_search`,
`cronjob_manage`, and `image_generate`, with the MCP allowlist inherited and
narrowed. Model routing mirrors the parent unless `delegation.model`,
`delegation.provider`, or `delegation.base_url` override it.

`finishUnit` is the single-winner commit: the worker and the stall monitor
race for the live-set transition, and the loser drops its result. `/new` and
`/stop` mark live units dropped (recorded honestly, marked delivered without
enqueueing); units already terminal before the cancel still deliver. Stall thresholds are 450s idle and
1200s in-tool with a 120s grace window; handoff retries cap at 20 attempts
before parking as failed. Recovery reaps rows whose owner died as `unknown` —
never retried as new work. Prune bounds the ledger (7-day retention, 1000-row
cap); live and pending rows are never touched.

## Tests

`delegate_test.go` with fake chatter/broker/processes and a real profile
store. Cover dispatch-to-delivery, steer/stop/list scoping (unknown ids report
the same error as finished or foreign work), `/new` drop semantics, schema
validation shapes, and the child block list.
