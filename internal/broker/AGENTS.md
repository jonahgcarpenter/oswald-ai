# broker/ — FIFO Conversation Lanes with Profile Fences and Steer

Applies on top of the root `AGENTS.md`.

## Shape

One package. `broker.go` holds the `Broker`, the `LaneKey{CanonicalUserID, SessionID}` map, and
the per-user fence table; `telemetry.go` emits `broker.request.execution.complete` and queue
snapshots. Production runs five workers (`brokerWorkerCount=5` in `startup/profiles.go`), so
unrelated lanes overlap while each lane stays strictly FIFO — the worker count is a RAM bound
(workers times per-turn footprint plus the bounded queue), with model-side queuing left to vLLM.

## How it works

Each lane is one user in one session. Plain messages steer into the running turn by default:
text-only follow-ups attach to the host's steer inbox and are consumed after the next tool
result, sharing the host response with no extra turn and no dead model call. `/queue` sets
`ForceQueue` and always waits as a FIFO follower. Stateless, media, and command lanes never
steer. Unconsumed steers auto-run as the next turn in arrival order; host failure requeues
every steer instead of delivering a partial answer. The fence is per profile name: ordinary turns take a shared read lock, while
`RunUsersExclusive` (commands, compression preemption checks) reserves a writer that waits for
active same-user work to finish and blocks new same-user readers. Reservations lock users in
sorted order so concurrent exclusives can't deadlock, and a panicking processor still releases
via `safeRun`.

Admission is bounded at queue 10 plus the workers (production capacity 15, counting
pending steers). Rejections deliver one
terminal result and return `ErrQueueFull`/`ErrShuttingDown` — and they still complete the
requested/completed pair, since `chat.requested` fires before admission. `CancelActiveAgentWork`
preempts only the running turn in one conversation while queued followers still run and steers
survive as queued next turns;
`CancelAllAgentWork` cancels agent turns and pending steers, leaving out-of-band commands untouched.
`TryAcquireLowPriority` succeeds solely on a fully idle broker and any enqueue revokes it, which
is how foreground work always preempts background compression. Shutdown drains admitted lane
followers before closing.

## Tests

`broker_test.go`, `fence_cancellation_test.go`, `telemetry_test.go`. Cover hot-lane fairness,
per-user fence exclusion, the capacity assertion (queue plus workers plus pending steers),
five-lane overlap with same-lane FIFO preserved for `/queue`, steer fan-out without an extra
model call, unconsumed-steer-becomes-next-turn, media/stateless never-steer, cancel-active
preserves steers vs cancel-all scope,
low-priority revocation, and drain on shutdown.
