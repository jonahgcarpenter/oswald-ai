# App - Composition, CLI, Commands

`internal/app/` owns process wiring and the operator surface. `startup` is the sole composition
root; `cli` is operator command dispatch; `commands` are chat-command adapters.

## Packages

- `startup` (`startup/`) — composition root: `Serve`/`Run`, profile assembly, gateway startup,
  ordered LIFO shutdown.
- `cli` (`cli/`) — top-level operator dispatch with exit-code mapping; injectable stdio.
- `setup`, `start` (`cli/setup/`, `cli/start/`) — `oswald setup` seed-only provisioning;
  `oswald start` foreground service wrapper.
- `profiles` (`cli/profiles/`) — `oswald profile create/delete` for one private profile subtree.
- `mcp` (`cli/mcp/`) — `oswald mcp auth <server>`: interactive OAuth authorization only.
- `commands` (`commands/`) — `Definition/Request/Result/Handler` contract, `/name` parsing,
  alias resolution, service dispatch.
- `builtin` (`commands/builtin/`) — registers the five public handlers and renders `/help`.
- `session` (`commands/session/`) — `/new`: close the open session, keep history searchable.
- `stop` (`commands/stop/`) — out-of-band `/stop`: cancel active agent work in the conversation.
- `approve` (`commands/approve/`) — out-of-band `/approve` + `/deny`: consume one pending
  five-minute ask.

## Rules

- No stores, workers, or gateways are assembled in `main` or `cli`. Only `cmd/*:main` exits.
- `setup` creates missing entries only and rolls back partial state on failure; it never
  overwrites. `profile delete` refuses `default` and requires `y`/`yes`.
- `/new` keeps transcripts, summaries, and locks; TTL expiry owns cleanup. It also denies pending
  approvals, as do restart and `/stop`.
- `/stop` preserves queued prompts; `/stop all` is rejected.
- `/approve` replays the identical invocation through the workspace sandbox; expiry, restart,
  `/new`, `/stop`, and the API auto-deny.
- Server binaries never import the `cli` tree.

## Testing

Startup tests inject clients/gateways, never start real listeners. Cover dispatch codes, seed-only
setup, create/delete guards, command aliasing, and shutdown order.
