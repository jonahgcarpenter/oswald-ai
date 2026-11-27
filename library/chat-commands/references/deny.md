# `/deny`

`Use: /deny` — deny the pending terminal command in this conversation.
Out-of-band: it runs even while the turn is parked on the approval.

## Effects

1. The parked tool call fails with a denial; the turn continues and the
   agent explains what was denied and proposes an alternative.
2. Like every other non-approval outcome, the pending approval is consumed —
   there is nothing left to approve afterwards.

## Lifespan

Single-use, same as `/approve`. After `/deny` the conversation proceeds
normally; the denied command is not retried unless the user explicitly
re-asks (which parks a fresh approval).

## Interactions

- `/deny` then `do it anyway`: treat as a new request — re-check, re-park,
  wait for `/approve`.
- Expiry, restart, `/new`, `/stop`, and API answers all deny implicitly;
  `/deny` is the explicit form with the same effect.

## Example

Assistant: *This will run `rm -rf ./build-cache` … reply `/approve` to allow it.*
User: `/deny`
Assistant: *Denied — I won't run that. Want me to list what's in build-cache first instead?*

## Deny cases

- Nothing pending: rejected, no-op.
