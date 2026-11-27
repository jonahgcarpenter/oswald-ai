# `oswald config`

Inspect configuration and validate files. **Read-only**: `show`, `get`,
`validate` never write. Reads take `--profile`.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `show` | `usage: oswald config show [--path P] [--profile NAME]` | whole file or one subtree, secrets redacted |
| `get` | `usage: oswald config get <path> [--profile NAME]` | one dotted leaf (e.g. `providers.fake.api`), redacted when secret |
| `validate` | `usage: oswald config validate [--profile NAME]` | runs the exact startup validation pipeline, reports `config_*` codes |

## Notes

- `validate` is the same gate every mutating command stages through — run it
  after any suspicious state before editing further.
- Interpolation is compose-style (`${VAR}`, `${VAR:-default}`,
  `${VAR:?err}`, `$$`); a bare missing variable fails closed naming only
  the path and variable. Process env wins over the profile `.env`.
- Files are bounded (256 KiB, no symlinks/aliases/duplicate/unknown keys,
  depth ≤32); dotenv files must be private regular files.

## Examples

```
oswald config validate
oswald config show --path model
oswald config get model.provider
oswald config get providers.fake.api
```

## Failures

- Invalid YAML, unknown keys, or failed interpolation: exit 1 with the
  classified code and schema path. Since nothing writes, retry is always safe.
