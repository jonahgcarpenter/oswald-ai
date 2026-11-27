# `oswald profile`

Create, rename, or delete private operator profiles. Each profile is an
island: its own `.env`, `config.yaml`, `state.db`, memories, and skills.
Reads and writes never cross profiles.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `create` | `usage: oswald profile create <name>` | scaffolds `profiles/<name>/` (private `.env`, minimal `config.yaml`, approved-schema `state.db`, `SOUL.md` copy) |
| `rename` | `usage: oswald profile rename <old> <new>` | renames the subtree; refused while `gateway.profile_routes` still target `<old>` |
| `delete` | `usage: oswald profile delete <name>` | removes the subtree and its data after explicit `y`/`yes` confirmation |

## Notes

- Names are bounded lowercase ASCII; `default` is reserved and can never be
  deleted (delete prompts, and empty input aborts).
- A fresh profile inherits every global provider, model, tool, and MCP
  default — its override document starts as `{}`. It can only add providers,
  model, tools, MCP servers, and `api`, never shared listeners or routes.
- Profile lifecycle requires the default scope.
- An empty named-profile `config.yaml` (`{}`) is valid, not an error.

## Examples

```
oswald profile create alice
oswald profile rename alice alicia
oswald profile delete alicia
# prompts: type y to confirm; anything else aborts
```

## Failures

- `rename` with live gateway routes targeting the profile: refused (move the
  routes first). Startup fails on dangling routes, so this guard is
  load-bearing.
- `delete default`: refused.
- Validation failures leave existing profiles untouched.
