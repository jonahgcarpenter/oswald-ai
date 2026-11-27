# `oswald platforms`

Manage gateway listeners (`platforms.*`): enable flags, listener/admission
leaves, and per-channel model routes. **Global-only, except
`channel-overrides`**, which is per-profile (global baseline plus overlays).

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `show` | `usage: oswald platforms show [name]` | section or one platform, secrets redacted |
| `enable` / `disable` | `usage: oswald platforms enable <discord\|bluebubbles\|api>` | flips `enabled`; idempotent (`already enabled/disabled`, exit 0) |
| `set` | `usage: oswald platforms set <discord\|bluebubbles\|api> [flags]` | listener/admission leaves (below) |
| `channel-overrides list` | `usage: oswald platforms channel-overrides list [--platform discord\|bluebubbles] [--profile NAME]` | effective routes for the calling scope |
| `channel-overrides add` | `usage: oswald platforms channel-overrides add --platform discord\|bluebubbles --channel ID --provider NAME --model MODEL [--profile NAME]` | upserts one route (`custom:` prefix accepted) |
| `channel-overrides edit` | `usage: oswald platforms channel-overrides edit --platform discord\|bluebubbles --channel ID [--provider NAME] [--model MODEL] [--profile NAME]` | changes one field of an existing route; omitted leaves keep current values |
| `channel-overrides remove` / `unset` | `usage: oswald platforms channel-overrides remove --platform discord\|bluebubbles --channel ID [--profile NAME]` | deletes the route (`unset` is an alias) |

## `set` flags

- `discord`: `--token REF`, `--require-mention true|false`, `--allow IDS`, `--ban IDS` (IDS comma-separated or repeatable).
- `bluebubbles`: `--server-url URL`, `--server-password REF`, listener flags (`--require-mention`, `--allow`, `--ban`, `--mention-pattern RE…`, `--webhook-host H`, `--webhook-port N`, `--webhook-path P`, `--webhook-callback-host H`).
- `api`: `--host IP`, `--port N`.

## Effects

`enable`/`disable` and overrides apply **live**. Listener payload changes
(hosts, ports, tokens, mention patterns) are **restart-class**. `add` on an
existing channel replaces it. Overrides validate against the providers
visible to the calling scope; a profile entry wins an id collision without
deleting the global one.

## Examples

```
oswald platforms show discord
oswald platforms enable discord
oswald platforms set discord --require-mention true --ban 555000111
oswald platforms channel-overrides add --platform discord --channel 55501 --provider fake --model fake/model
oswald platforms channel-overrides edit --platform discord --channel 55501 --model fake/other
oswald platforms channel-overrides remove --platform discord --channel 55501
```

## Failures

- Unknown platform (only `discord|bluebubbles` carry routes; `api` never
  does), ghost provider, bad channel id (>256 runes), or editing a missing
  entry: usage/exit-1, disk unchanged.
