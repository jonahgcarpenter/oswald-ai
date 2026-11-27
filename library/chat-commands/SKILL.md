---
name: chat-commands
description: Explain slash commands (/new, /stop, /approve, /model).
version: 1.0.0
---

# Chat Commands

Explain the in-chat slash commands when the user asks about them or when one
of them is the right tool for what the user wants. This skill is for
**telling**: slash commands are chat input, not terminal commands — the user
runs them, never you. Do not "execute" them yourself and never approve on the
user's behalf; suggest the command and let the user send it.

## Command map

| Command | Does | Reference |
|---|---|---|
| `/help` | list commands or usage for one | `references/help.md` |
| `/new` | end the session, start fresh | `references/new.md` |
| `/stop` | cancel the running turn | `references/stop.md` |
| `/approve` | approve the pending terminal command | `references/approve.md` |
| `/deny` | deny the pending terminal command | `references/deny.md` |
| `/compact` | summarize older turns now | `references/compact.md` |
| `/queue` | force the next prompt to queue | `references/queue.md` |
| `/model` | show, list, or pin the conversation model | `references/model.md` |

## Persistent overview (true for every command)

- **Pins are per-conversation.** A `/model <provider>/<model>` pin lives and
  dies with the conversation; only `/model ... --global` persists, by writing
  the profile config through the mediated path (applies live on the next
  watcher tick).
- **Generations fence history.** `/new` bumps the generation; old turns stay
  searchable under the previous generation. Delivery gating is intentional —
  a turn from a stale generation never delivers.
- **Approvals are single-flight with a five-minute window.** Expiry, restart,
  `/new`, `/stop`, and the API all deny. `/approve` replays the identical
  terminal invocation through every check; anything else denies.
- **Steer vs queue.** Plain follow-ups steer into the running turn (text-only,
  consumed after the next tool result); `/queue` forces FIFO as the next turn.
  Unconsumed steers auto-run as the next turn instead of firing an unchanged
  call.
- **Delegates follow the conversation.** `/new` and `/stop` interrupt live
  delegated children; their partial results are dropped, never delivered.
