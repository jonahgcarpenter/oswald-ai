# `/new`

`Use: /new` — end the open session (`end_reason='new_session'`) and start
fresh. Past sessions stay searchable.

## Effects

1. The current session closes; a new one opens on the next message.
2. The generation counter bumps — delivery gating keys on it, so nothing
   from the old generation can deliver into the new one.
3. Pending terminal approvals are denied.
4. Live delegated children are interrupted; their partial results are
   dropped, never delivered.
5. History stays searchable under the old generation (sessions are never
   deleted by `/new`).

## Lifespan

The pin, steers, and queue belong to the old conversation and do not carry
over — except a `/model` pin, which *does* survive `/new` (it is bound to
the conversation identity the user keeps). State set by `/model --global`
lives in config, not the session, so it persists trivially.

## Interactions

- `/new` then an unrelated question: the model answers with no memory of the
  previous session beyond what's searchable.
- `/new` during a pending approval or a running turn: both are dropped
  first, then the session resets. Nothing half-applied leaks across.

## Example

User: `/new` → *fresh session. What were we doing?*
Assistant: answers from the new (empty) session; offers to look up the old
session's history if the user wants continuity.

## Deny cases

- None — `/new` always succeeds. Data loss is limited to the open turn's
  unsent work, which is announced, not hidden.
