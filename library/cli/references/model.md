# `oswald model`

Manage `model.*`: the acting profile's provider, default model, and context
window. Reads take `--profile`; writes fence to the calling scope.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `show` | `usage: oswald model show [--profile NAME]` | the `model` section |
| `list` | `usage: oswald model list [--profile NAME]` | live models per provider (8s timeout each, 4 in flight; dead endpoints print `unavailable`, never error) |
| `set` | `usage: oswald model set [--provider P] [--default M] [--context-length N] [--profile NAME]` | exactly one field per call |
| `edit` | `usage: oswald model edit [--provider P] [--default M] [--context-length N] [--profile NAME]` | alias of `set` |
| `unset` | `usage: oswald model unset context-length [--profile NAME]` | clears `model.context_length` only |

## Notes

- `--provider` takes the `custom:<name>` selector (`custom:fake`), naming an
  entry in `providers.*`.
- `provider` and `default` are **required** — startup fails without them, so
  `unset` rejects both (`use 'oswald model set …' to change it`). Only
  `context_length` is optional.
- In-chat `/model <provider>/<model>` pins the conversation only; `--global`
  performs this `set` through the mediated path and applies live on the next
  watcher tick.

## Effects

Provider/default changes apply **live**. Context-length changes are
**restart-class** (`model_window`).

## Examples

```
oswald model show
oswald model list
oswald model set --provider custom:fake
oswald model set --default fake/model
oswald model edit --context-length 65536
oswald model unset context-length
```

## Failures

- Multi-flag `set`/`edit`, unknown provider, or `unset provider|default`:
  usage errors. Failed validation leaves the file byte-identical.
