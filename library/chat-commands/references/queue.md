# `/queue`

`Use: /queue <prompt>` — queue a prompt for the next turn instead of
steering the running turn.

## Effects

1. Sets `ForceQueue`: the prompt waits as a FIFO follower and runs as its
   own turn after the current one finishes.
2. Never shares the host response — a queued turn always gets its own model
   call and its own answer.

## Lifespan

Queue order is arrival order per lane; queued prompts survive `/stop`
(which cancels only the running turn). `/new` ends the session, and with it
the queue.

## Interactions

- Plain messages steer by default: text-only, attached to the host's steer
  inbox, consumed after the next tool result, sharing the host response with
  no extra model call. Media that would overflow the per-request image bound
  stays pending and auto-runs as the next turn instead.
- Unconsumed steers auto-run as the next turn rather than firing an
  unchanged-prompt call; on host failure every steer requeues instead of
  sharing a partial answer.
- Suggest `/queue` when the user wants to line up follow-ups without
  perturbing the turn that's already running.

## Example

User: (mid-run) `/queue now check the error log too`
Assistant: finishes the running turn, then runs the log check as its own
turn with its own answer.

## Deny cases

- Empty prompt after `/queue`: nothing to queue. A queued prompt is never
  silently dropped — it runs, or the session ends first.
