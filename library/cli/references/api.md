# `oswald api`

Manage the OpenAI gateway credential (`api.key`). Reads take `--profile`;
writes fence to the calling scope.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `show` | `usage: oswald api show [--profile NAME]` | credential presence, key redacted |
| `set-key` | `usage: oswald api set-key --key REF [--profile NAME]` | sets the key (literal or `${VAR}` reference) |
| `clear-key` | `usage: oswald api clear-key [--profile NAME]` | removes the key |

## Notes

- `--key` accepts a credential literal or a `${VAR}` interpolation resolved
  against the profile environment at runtime. Prefer the reference form so
  the secret lives in the private `.env`, never in `config.yaml`.
- The OpenAI gateway is text-only: no commands (a leading `/` is a client
  `400`), no persistence, no attachments.

## Effects

Credential changes apply **live** (key is a routing-class leaf).

## Examples

```
oswald api show
oswald api set-key --key '${OPENAI_API_KEY}'
oswald api clear-key
```

## Failures

- Missing `--key`, or unresolvable `${VAR}` at validation: exit 1 with a
  `config_*` diagnostic; file left byte-identical.
