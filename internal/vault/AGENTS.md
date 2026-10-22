# vault/ — Profile-Scoped Encrypted Credential Store

Applies on top of the root `AGENTS.md` (operator-state and never-log rules).

## Shape

`store.go` is an AES-256-GCM file store under `<profileRoot>/vault/`
(`vault.key` + `vault.json.enc`, both `0600`, flock-serialized). Login and
address items are fillable; payment items can be stored and listed but
never filled (card entry needs an interactive confirmation no session
provides, so filling refuses fail-closed).
`filljs.go` builds the page scripts (nonce-stamped inspection, origin-bound
fill) and classifies controls in Go. `redact.go` is the process-wide secret
registry: resolved values are registered before they touch a page and later
browser output is scrubbed through it.

## Rules

- Metadata (label, origin, login identifier) is agent-visible; secrets
  (passwords, authenticator seeds, minted codes) never enter tool results,
  logs, errors, or history. Scrub every error path with `ScrubValues`.
- The fill origin check runs twice: Go pre-check for a fast refusal, then
  the synchronous in-page assertion (the load-bearing one). Exact origin
  match only — nothing wildcard or parent-domain is ever inferred.
- `providers/browser` may import this package (state, not tools); tool
  packages must not be imported from here.
