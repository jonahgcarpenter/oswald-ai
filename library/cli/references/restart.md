# `oswald restart`

Signal the local `oswald-server` to reboot in place (the supervisor
restarts it). The first `SIGTERM` **drains** instead of killing: new prompts
get a retry notice while in-flight turns run to a bounded grace.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `restart` | `usage: oswald restart [--delay SECS\|--now] [--force]` | signals the server; prints `The server will restart after all responses have been sent (pid N)` and exits 0 immediately (fire-and-forget) |

## Options

| Flag | Values | Notes |
|---|---|---|
| `--delay SECS` | `0`–`300` | wait before signaling; `--now` is zero delay |
| `--force` | — | sends `SIGKILL`: immediate death, **skips the drain**; for a wedged server only |

## Drain behavior

1. Cron tickers stop (no new scheduled work; drain rejections never count
   toward a job's failure streak).
2. The broker stops admission: every new prompt or steer is rejected with
   `Oswald is restarting, please retry in a couple of seconds`.
3. Admitted turns keep their workers, tools, MCP sessions, and gateway
   outbound, and deliver normally.
4. When the broker goes idle (or the ~5-minute grace expires, or a second
   signal arrives), normal teardown runs and the process exits.

## Scope

No `--profile`: restart is process-wide. Requires the server to be running
(`oswald: server is not running`, exit 1, otherwise). Discovery uses
`/proc` (`exe` basename, falling back to `comm`) — nearest
`oswald-server` ancestor, else PID 1 when the CLI runs inside the server.

## Examples

```
oswald restart
# The server will restart after all responses have been sent (pid 1234)

oswald restart --delay 60
# signals in 60 seconds (batch window before a breaking change lands)

oswald restart --force
# SIGKILL: no drain, in-flight turns die
```

## Failures

- Usage errors (bad `--delay`, unknown flags): exit 2 with the usage line.
- A turn still running at grace expiry is canceled silently — its user
  retries after the reboot. The guarantee is bounded, not unbounded.
