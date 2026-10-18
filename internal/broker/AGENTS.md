# Broker - Conversation Lanes

`internal/broker/` owns FIFO per-conversation scheduling with profile fences, bounded admission,
cancellation, and one low-priority compression permit. One worker in production.

## Layout

- `broker.go` — `Broker`, `LaneKey{CanonicalUserID,SessionID,UserExclusive}`, `Submit`,
  `RunInLane`, `RunUsersExclusive`, `TryAcquireLowPriority`, `Shutdown`, `CancelActiveAgentWork`,
  `CancelAllAgentWork`. Errors `ErrQueueFull/ErrShuttingDown/ErrAgentWorkCanceled`.
- `telemetry.go` — `broker.request.execution.complete` plus `Snapshot()` queue depth.

## Rules

- Lane = user + session. Same-lane work runs FIFO; different lanes run concurrently up to the
  worker count. A hot lane never starves other lanes.
- Fences are per `CanonicalUserID`: normal work takes a shared reader; `RunUsersExclusive` takes a
  writer that waits for active same-user work and blocks new same-user readers. Other users are
  unaffected. Reserve users in sorted order to avoid deadlock.
- Admission is bounded: queue 10 + worker count (production capacity 11). Rejections deliver one
  terminal result and return an error; they still produce a `chat.requested`/`chat.completed` pair.
- `CancelActiveAgentWork` preempts only the running turn in one conversation; queued followers still
  run. `CancelAllAgentWork` cancels agent turns only — out-of-band commands survive.
- `TryAcquireLowPriority` succeeds only when the broker is fully idle; any enqueue revokes it.
  Foreground work always preempts background compression.
- `Shutdown` drains accepted lane followers before closing; never drop admitted work silently.

## Testing

Cover hot-lane fairness, fence exclusion per user, queue-full rejection shape, cancel-active vs
cancel-all scope, low-priority revocation, and drain-on-shutdown.
