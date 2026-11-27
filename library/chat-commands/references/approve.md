# `/approve`

`Use: /approve` — approve the pending terminal command in this conversation.
Out-of-band: it runs even while the turn is parked on the approval.

## Effects

1. Replays the **identical** terminal invocation through lifecycle,
   self-repo, sandbox, and the config/fetch re-checks. Destructive-command
   detection is skipped by explicit user override — approval means the user
   accepted the risk.
2. On success the parked tool call resumes with the approved command.
3. Anything else denies: expiry past five minutes, restart, `/new`, `/stop`,
   or the API answering instead of the user.

## Lifespan

Single-flight and single-use: one pending approval per conversation, consumed
by the first `/approve` or `/deny`. A fresh risky command parks a fresh
approval.

## Interactions

- `/approve` after editing the command in chat: denied — it must be the
  identical invocation. The user edits, the agent re-proposes, the user
  approves the new text.
- `/approve` with nothing pending: rejected, no-op.
- Approvals never cross conversations or profiles, and never survive a
  restart (in-memory only).

## Example

Assistant: *This will run `rm -rf ./build-cache` … reply `/approve` to allow it (5 min).*
User: `/approve`
Assistant: replays the checks, runs it, reports the result.

## Deny cases

- Past the 5-minute window, after any `/new`, `/stop`, or server restart, or
  from the API: treated as denied. The agent must say what was denied and
  re-propose rather than silently retrying.
