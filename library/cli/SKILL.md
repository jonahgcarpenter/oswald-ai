---
name: cli
description: Run oswald CLI for models, providers, profiles, config.
version: 1.0.0
---

# Oswald CLI

Run the `oswald` operator CLI through the terminal on behalf of the chatting
user. This skill is for **doing**: read the entry, open exactly the
`references/<group>.md` file you need, run the command, report the result.
To explain the CLI without running it, quote the reference file instead.

## Scope rule (read first)

Never pass `--profile`. The terminal already runs scoped to the chatting user
(`OSWALD_HOME` is fixed to their profile), and naming any other profile fails
closed on both reads and writes. Run bare commands; they act as the chatting
user automatically.

Global-only groups (`gateway`, `platforms` except `channel-overrides`,
`runtime`) additionally require the default scope. Under a named profile they
are denied — report that instead of retrying.

## Procedure

1. Match the request to one group in the map below and read its reference file.
2. Read before writing (`show`, `list`, `status`, `test`, `validate`).
3. Write, then read the effect line: `applied live` needs nothing further;
   `restart required` means batch all remaining edits first, then run
   `oswald restart` once. Restarts drain in-flight turns (see
   `references/restart.md`) — never treat `restart required` as urgent.
4. Verify with `show` or `list`.

## Group map

| Group | Manages | Reference |
|---|---|---|
| `setup` | first-install root seeding | `references/setup.md` |
| `restart` | drain-and-reboot the server | `references/restart.md` |
| `profile` | profile create/rename/delete | `references/profile.md` |
| `mcp` | profile MCP servers + OAuth | `references/mcp.md` |
| `gateway` | profile routing (multiplex + routes) | `references/gateway.md` |
| `platforms` | listeners, admission, channel routes | `references/platforms.md` |
| `providers` | LLM providers + subscription auth | `references/providers.md` |
| `model` | model selection | `references/model.md` |
| `delegation` | subagent model routing | `references/delegation.md` |
| `api` | OpenAI gateway credential | `references/api.md` |
| `tools` | tool backends (search/image/browser/terminal) | `references/tools.md` |
| `plugins` | per-profile plugin installs | `references/plugins.md` |
| `skills` | per-profile skill installs | `references/skills.md` |
| `runtime` | log level and runtime config | `references/runtime.md` |
| `config` | read-only inspection | `references/config.md` |
| `env` | private profile environment | `references/env.md` |

`help` is a flag, not a group: every group answers `help`, `-h`, `--help`,
and every leaf answers `<command> --help`, all on stdout with exit 0 and
without touching scope.

## Persistent overview (true for every group)

- **Writes are mediated.** Each mutation is staged through the exact startup
  validation pipeline in a temp root; only on success are the previous bytes
  kept as last-known-good and the candidate atomically written. A rejected
  edit leaves the file byte-identical and reports a classified `config_*`
  diagnostic naming only the schema path, artifact, and variable — never
  values.
- **Live vs restart.** Hot classes apply live with no restart: `runtime` log
  level, gateway admission policy, `model` defaults, `providers` transports,
  and `channel_overrides`. Everything else — context windows, profile
  additions/removals, listeners, MCP sessions, plugins, tool backends, and
  the remaining sections — prints `restart required - run 'oswald restart'
  to apply` and takes effect after the reboot.
- **Exit codes.** `0` ok, `1` config/runtime error, `2` usage. Usage errors
  print the leaf usage to stderr.
- **Set/unset discipline.** `set`/`edit` take exactly one field per
  invocation; `unset` clears one leaf back to inherit. Whole-entry deletion
  is `remove`.
- **Secrecy.** Never log, quote, or echo prompts, answers, credentials,
  dotenv contents, URLs, session keys, or raw errors. Keys print as
  `[redacted]`; keep them that way.
