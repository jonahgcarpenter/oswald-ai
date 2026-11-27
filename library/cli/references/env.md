# `oswald env`

Manage the private profile environment (the profile `.env`, `0600`). Reads
take `--profile`; writes fence to the calling scope. Secrets travel on argv
and are never echoed or logged.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `show` | `usage: oswald env show [--profile NAME]` | variable names (values stay private) |
| `set` | `usage: oswald env set <NAME> <VALUE> [--profile NAME]` | sets one variable, private file, atomic replace |
| `unset` | `usage: oswald env unset <NAME> [--profile NAME]` | removes one variable |

## Notes

- Named profiles inherit the default `.env` and override per key; the
  process environment wins over both at runtime without mutating files.
- `${VAR}` references in `config.yaml` (provider keys, tokens, API keys)
  resolve against this environment — prefer references over literals so
  secrets never land in `config.yaml`.
- A dotenv change re-validates on the next watcher tick; credential
  rotations apply live, structural problems log `config.reload.rejected`
  and keep the live config.

## Examples

```
oswald env show
oswald env set OPENAI_API_KEY 'sk-...'
oswald env unset OLD_TOKEN
```

## Failures

- Invalid names or an unruly file (symlink, wrong perms): exit 1, file
  untouched. `set`/`unset` go through the same staged-validation path as
  every other mutation.
