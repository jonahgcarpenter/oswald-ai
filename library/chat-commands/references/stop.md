# `/stop`

`Use: /stop` — cancel the running agent turn in this conversation. Queued
follow-ups are preserved. Out-of-band: it runs even while a turn is active.

## Effects

1. Cancels the currently running foreground work in this conversation.
2. Interrupts live delegated children too; their partial results are
   dropped, never delivered.
3. Preserved: queued prompts still run as the next turns; steered follow-ups
   already consumed stay consumed, unconsumed steers auto-run as next turns.

## Lifespan

Cancellation is terminal for the interrupted turn — no partial answer
delivers afterwards. The session, pin, and queue are untouched.

## Interactions

- `/stop` then silence: the next user message starts a normal new turn.
- `plain message` during a run steers by default (it does *not* stop); only
  `/stop` cancels. `/stop all` is rejected — scope is always this
  conversation.
- `/stop` during a pending approval denies it (approvals die on stop, like
  on `/new`, restart, and expiry).

## Example

User: (while the agent is mid-tool-loop) `/stop` → *Stopped. Anything else?*
User: `actually just summarize what you found`
Assistant: summarizes from the persisted partial history without re-running
the canceled tools, and says so.

## Deny cases

- Nothing running: harmless no-op. `/stop all`: rejected outright.
