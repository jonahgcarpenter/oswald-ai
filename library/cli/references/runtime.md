# `oswald runtime`

Read and safely modify runtime configuration (`runtime.*`: log level and
bounds). **Global-only** (no `--profile`): requires the default scope.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `show` | `usage: oswald runtime show` | the runtime section |
| `set` | `usage: oswald runtime set --log-level LEVEL (debug\|info\|warn\|error)` | sets the log level |

## Effects

Log level applies **live** (the running server re-levels on the next watcher
tick). Other runtime leaves are **restart-class**.

## Examples

```
oswald runtime show
oswald runtime set --log-level debug
# …reproduce, then…
oswald runtime set --log-level info
```

## Failures

- Non-default scope: denied. Unknown level: usage error (exit 2).
  Validation failures never touch disk.
