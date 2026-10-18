# App - Composition, CLI, Commands

`internal/app/` owns process wiring and the operator surface. `startup` is the sole composition
root; `cli` is operator command dispatch; `commands` are chat-command adapters. Only `cmd/*:main`
exits; everything here returns errors after cleanup.

## Layout

- `startup/serve.go` — `Serve(ctx,root,stdout)`: load profiles, print banner if TTY, install
  `signal.NotifyContext`, run, log classified failure. Never `os.Exit`.
- `startup/profiles.go` — `runProfilesWith()`: per-profile stores, tools, LLM, compactor, MCP,
  agent, prune worker, cron; then shared broker, profile commands, compression workers, gateways.
  Deferred LIFO shutdown: outbound → prune → maintenance → broker → compression → cron ticker →
  `process.StopAll` → MCP close → stores → `app.stopped`.
- `startup/app.go`, `startup/banner.go` — `Run()` wrapper, `Error{Event,Message,Cause}`, TTY-gated
  banner.
- `cli/cli.go` — top-level dispatch (`setup|start|profile|mcp|help`), exit codes 0/1/2.
- `cli/setup/`, `cli/start/`, `cli/profiles/`, `cli/mcp/` — seed-only setup, start wrapper,
  profile create/delete (refuses `default`, requires `y/yes`), `mcp auth`.
- `commands/` — `types.go`/`parser.go`/`service.go` shared contract; `builtin/` registers the five
  public handlers; `session/` owns `/new`; `stop/` owns out-of-band `/stop`; `approve/` owns
  out-of-band `/approve` + `/deny`.

## Rules

- No stores, workers, or gateways are assembled in `main` or `cli`. All wiring lives in `startup`.
- `setup` creates missing entries only, never overwrites. Profile create rolls back partial state.
- `/new` is bookkeeping: close the open session, keep history searchable, deny pending approvals.
- `/stop` cancels active agent work in the conversation, preserves queued prompts. `/stop all` is
  rejected.
- `/approve` replays the identical invocation through the workspace sandbox; single-flight pending
  ask, five-minute window, expiry/restart/`/new`/`/stop`/API auto-deny.
- Server binaries never import the `cli` tree.

## Testing

Startup tests inject clients/gateways, never start real listeners. Cover dispatch exit codes,
seed-only setup, profile create/delete guards, command aliasing, and shutdown ordering.
