# `oswald delegation`

Manage `delegation.*`: subagent (delegate) model routing. Unset leaves mirror
the profile model with default bounds. Reads take `--profile`; writes fence
to the calling scope.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `show` | `usage: oswald delegation show [--profile NAME]` | the section, values only (absent section is valid: delegates mirror the profile model) |
| `set` | `usage: oswald delegation set [--model M \| --provider P \| --base-url URL \| --max-iterations N \| --max-children N \| --max-depth N \| --orchestrator true\|false] [--profile NAME]` | exactly one field per call |
| `edit` | same flags as `set` with `edit` in place of `set` | alias of `set` |
| `unset` | `usage: oswald delegation unset model\|provider\|base-url\|max-iterations\|max-children\|max-depth\|orchestrator [--profile NAME]` | clears one leaf back to inherit (`_` spellings accepted) |

## Bounds

| Flag | Leaf | Range |
|---|---|---|
| `--max-iterations` | `max_iterations` | `1`–`10000` (default 250) |
| `--max-children` | `max_concurrent_children` | `1`–`32` (default 10) |
| `--max-depth` | `max_spawn_depth` | `1`–`8` (default 1) |
| `--orchestrator` | `orchestrator_enabled` | `true`/`false` (default true) |

`--base-url` requires `--provider` (an endpoint override is meaningless
without a named provider) and must be `http(s)` with no userinfo, query, or
fragment. `--model`/`--provider` empty means inherit.

## Effects

Routing/bound changes apply **live** (delegate live configs swap after the
Directory swap; an applier failure WARNs and keeps the previous runtime
view).

## Examples

```
oswald delegation show
oswald delegation set --provider other
oswald delegation set --base-url https://delegate.example.com/v1
oswald delegation edit --max-iterations 40
oswald delegation unset base-url
```

## Failures

- Multi-flag calls, out-of-range bounds, `--orchestrator maybe`, or
  `--base-url` without a provider: usage/validation errors, disk unchanged.
- Unsetting a never-set bound leaf errors (nothing to clear).
