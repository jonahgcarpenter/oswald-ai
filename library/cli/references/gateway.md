# `oswald gateway`

Manage profile routing: multiplexing plus the `profile_routes` table that
maps transport identities to profiles. **Global-only** (no `--profile`):
reads and writes require the default scope.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `show` | `usage: oswald gateway show` | `gateway.multiplex_profiles` + routes, secrets redacted |
| `set-multiplex` | `usage: oswald gateway set-multiplex <true\|false>` | explicit default routes need no multiplexing; named profiles do |
| `routes list` | `usage: oswald gateway routes list` | every route entry |
| `routes add` | `usage: oswald gateway routes add --name N --platform discord\|bluebubbles\|* --user-id ID\|* --profile P` | adds a route (`--profile` here is the *target* profile, not scope) |
| `routes remove` | `usage: oswald gateway routes remove <name>` | deletes a route by name |

## Routing semantics

Matching is most-specific first: exact platform+user, then per-platform
wildcard (`{"discord","*"}`), then global wildcard (`{"*","*"}`), then the
implicit default fallback. `platform:"*"` with a concrete user is rejected
as ambiguous. Catch-alls only choose the profile — admission (bans first,
then allow-lists) still runs and can reject.

## Effects

Gateway changes are **restart-class**. A route naming a nonexistent profile
fails validation (startup fails on dangling routes).

## Examples

```
oswald gateway show
oswald gateway set-multiplex true
oswald gateway routes add --name alice-dm --platform discord --user-id 555000111 --profile alice
oswald gateway routes remove alice-dm
```

## Failures

- Non-default scope: denied, exit 1. Unknown platform/user patterns or a
  missing `--profile` target: usage/validation errors, disk unchanged.
