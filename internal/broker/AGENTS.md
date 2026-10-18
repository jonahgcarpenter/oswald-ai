# Broker - Conversation Lanes

`internal/broker/` owns FIFO per-conversation scheduling with profile fences, bounded admission,
cancellation, and one low-priority compression permit. One worker in production.

## Packages

- `broker` (single package) — `Broker` and `LaneKey` scheduling (`broker.go`): lane queues, user
  fences, admission caps, cancellation, `RunInLane`/`RunUsersExclusive`/`TryAcquireLowPriority`,
  shutdown drain. Execution telemetry and queue snapshots (`telemetry.go`).

## Rules

- Lane is user + session: same-lane work runs FIFO; different lanes run concurrently up to the
  worker count.
- Fences are per profile name: normal work takes a shared reader; `RunUsersExclusive` waits for
  active same-user work and blocks new same-user readers. Reserve users sorted to avoid deadlock.
- Admission is queue 10 + worker count (production capacity 11). Rejections deliver one terminal
  result and return an error; they still complete the requested/completed pair.
- `CancelActiveAgentWork` preempts only the running turn in one conversation; queued followers
  still run. `CancelAllAgentWork` cancels agent turns only — out-of-band commands survive.
- `TryAcquireLowPriority` succeeds only on a fully idle broker; any enqueue revokes it, so
  foreground work always preempts background compression.

## Testing

Cover hot-lane fairness, fence exclusion per user, queue-full rejection shape, cancel-active vs
cancel-all scope, low-priority revocation, and drain on shutdown.
