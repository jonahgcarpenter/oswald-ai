# `oswald mcp`

Manage profile MCP servers (`mcp_servers.*`): add/edit/remove through staged
validation, liveness `test`, and OAuth `auth`. `mcp auth` is the only
interactive OAuth path.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `list` | `usage: oswald mcp list [--profile NAME]` | configured servers with transport, auth, and token state |
| `add` | `usage: oswald mcp add <server> [--profile NAME] (--url URL \| --command CMD) [options]` | adds a server; `--enable`/`--disable` are mutually exclusive |
| `edit` | `usage: oswald mcp edit <server> [--profile NAME] [field flags]` | merges fields over an existing server |
| `remove` | `usage: oswald mcp remove <server> [--profile NAME] [--full]` | deletes the server (its directory is kept unless `--full`) |
| `test` | `usage: oswald mcp test <server> [--profile NAME] [--timeout DURATION]` | dials once and lists tools |
| `auth` | `usage: oswald mcp auth <server> [--flow browser\|device] [--no-browser] [--timeout DURATION] [--profile NAME]` | OAuth authorization flow |

## Options (add/edit)

Transport (exactly one for `add`): `--url URL` (remote) or `--command CMD`
(stdio) with repeatable `--arg`, `--cwd`, `--env KEY=VAL`, `--header H:V`.
`--enable` / `--disable` flip the server; `--no-env`, `--no-header`,
`--no-oauth` drop those blocks. Secrets travel on argv and are never echoed
or logged — same posture as `oswald env set`.

## Effects

MCP session changes are **restart-class**: `added … (restart required …)`.
`auth`/`test` are read-only checks. Auth state is per profile; named
profiles fence both reads and writes to their own scope.

## Examples

```
oswald mcp list
oswald mcp add gh --url https://mcp.example.com --enable
oswald mcp edit gh --env GITHUB_TOKEN='${GH_TOKEN}'
oswald mcp test gh --timeout 30s
oswald mcp auth gh --flow device
oswald mcp remove gh
```

## Failures

- Unknown server, bad transport, or failed validation: exit 1 with a
  classified diagnostic; disk left byte-identical.
- OAuth is interactive: `--no-browser` prints the URL instead of opening one
  (for headless handoffs).
