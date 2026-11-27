# `oswald plugins`

Manage per-profile plugin installs (`plugins.entries.*`). Presence means
enabled — there is no enable flag. Reads take `--profile`; writes fence to
the calling scope.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `list` | `usage: oswald plugins list [--profile NAME]` | installed plugins |
| `info` | `usage: oswald plugins info <name> [--profile NAME]` | manifest + install metadata |
| `install` | `usage: oswald plugins install <source> [--ref SHA] [--subdir PATH] [--node-package PKG --node-version VER] [--no-deps] [--force] [--profile NAME]` | installs from URL or local path (git shorthand/URLs accepted) |
| `update` | `usage: oswald plugins update <name> [--ref SHA] [--profile NAME]` | refreshes to a new rev |
| `remove` | `usage: oswald plugins remove <name> [--full] [--profile NAME]` | removes (code kept unless `--full`) |

## Notes

- Sources stage through quarantine (manifest validation, atomic publish)
  with a `.install-metadata.json` provenance sidecar per plugin.
- The node sidecar builds on install unless `--no-deps`; `--node-package`
  + `--node-version` pin it. Never `npm install` by hand — the installer
  owns the sidecar.
- A plugin's skills appear in that profile's skill index once installed;
  `skill_manage view` reads them like native skills.

## Effects

Plugin changes are **restart-class** (the watcher only logs
`config.reload.restart_required` for plugin markers).

## Examples

```
oswald plugins list
oswald plugins install gh:owner/repo --subdir plugins/foo
oswald plugins info foo
oswald plugins update foo --ref abc1234
oswald plugins remove foo
```

## Failures

- Bad source, manifest validation failure, or unknown name: exit 1 with a
  classified diagnostic; nothing is half-published (atomic publish or
  nothing).
