# broker/ — FIFO Conversation Lanes with Profile Fences

Applies on top of the root `AGENTS.md`.

## Shape

One package. `broker.go` holds the `Broker`, the `LaneKey{CanonicalUserID, SessionID}` map, and
the per-user fence table; `telemetry.go` emits `broker.request.execution.complete` and queue
snapshots. Production runs five workers (`brokerWorkerCount=5` in `startup/profiles.go`), so
unrelated lanes overlap while each lane stays strictly FIFO — the worker count is a RAM bound
(workers times per-turn footprint plus the bounded queue), with model-side queuing left to vLLM.

## How it works

Each lane is one user in one session, strictly FIFO — the head runs, followers wait, and a hot
lane never occupies the worker while queued. Different lanes proceed concurrently up to the worker
count. The fence is per profile name: ordinary turns take a shared read lock, while
`RunUsersExclusive` (commands, compression preemption checks) reserves a writer that waits for
active same-user work to finish and blocks new same-user readers. Reservations lock users in
sorted order so concurrent exclusives can't deadlock, and a panicking processor still releases
via `safeRun`.

Admission is bounded at queue 10 plus the workers (production capacity 15). Rejections deliver one
terminal result and return `ErrQueueFull`/`ErrShuttingDown` — and they still complete the
requested/completed pair, since `chat.requested` fires before admission. `CancelActiveAgentWork`
preempts only the running turn in one conversation while queued followers still run;
`CancelAllAgentWork` cancels agent turns only, leaving out-of-band commands untouched.
`TryAcquireLowPriority` succeeds solely on a fully idle broker and any enqueue revokes it, which
is how foreground work always preempts background compression. Shutdown drains admitted lane
followers before closing.

## Tests

`broker_test.go`, `fence_cancellation_test.go`, `telemetry_test.go`. Cover hot-lane fairness,
per-user fence exclusion, the capacity assertion (queue plus workers), five-lane overlap with
same-lane FIFO preserved, cancel-active vs cancel-all scope,
low-priority revocation, and drain on shutdown.
