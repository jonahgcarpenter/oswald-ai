# Oswald AI - Uncensored Digital Servant

> Fully local, fully uncensored, with no paid API required.

## Overview

Oswald AI is a local-first, self-hosted assistant that brings your chosen language model to iMessage, Discord, and a OpenAI-compatible API.
It combines tools, private profile memory, conversation continuity, image understanding, and connected services while keeping you in control of your data.

A harness can't make a model uncensored — that property lives in the model weights and how it was trained, not in the code around it. What the harness can do is stay out of the way: Oswald adds no content filters, no moderation layers, and no safety moralizing of its own, so whatever model you point it at speaks exactly as it was built to. It was designed with running an uncensored model in mind, and self-hosting means no provider ever stands between you and your model.

## Features

- Chat through iMessage, Discord, or the OpenAI-compatible API
- Send text, images, animated GIFs, and replies with quoted context
- Search the web, generate and edit images, and use connected MCP tools
- Remember your preferences, projects, and other useful details across conversations
- Keep continuity in long conversations and search earlier conversation details

## Usage

### Discord/iMessage Bot

In DMs or direct chats, send any message:

```text
What is the current weather?
```

In server channels or group chats, start your message with a mention of Oswald:

```text
@Oswald What is the capital of France?
```

You can also reply to any message and mention Oswald to include that message as context:

```text
[Replying to Jonah: "The capital of the US is New York"]
@Oswald Is this true?
```

Recognized direct replies to Oswald can continue the conversation without another mention:

```text
[Replying to Oswald: "The Cincinnati Bengals are easily the best team in the NFL"]
Can you elaborate on that?
```

### API

Oswald also serves an OpenAI-compatible API on loopback by default (`127.0.0.1:8000`,
configurable via `api_host`/`api_port`), authenticated per profile with the bearer key in
`OPENAI_API_KEY`:

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model": "oswald", "messages": [{"role": "user", "content": "Hello Oswald"}]}'
```

Conversations over the API are stateless — nothing is stored — and responses are text-only:
no attachments and no slash commands.

To use Oswald as a Home Assistant voice assistant, see
[has-oswald-conversation](https://github.com/jonahgcarpenter/has-oswald-conversation)

## Chat Commands

Chat commands are gateway-level slash commands. They are handled before requests reach the model.

In Discord servers and iMessage groups, slash commands must start with a mention of Oswald.

| Command    | Usage             | Description                                                                                            |
| ---------- | ----------------- | ------------------------------------------------------------------------------------------------------ |
| `/help`    | `/help [command]` | List available commands or show usage for one command.                                                 |
| `/new`     | `/new`            | Start a fresh session with the latest memory files; past conversations remain searchable until expiry. |
| `/stop`    | `/stop`           | Stop the currently running response in this conversation without removing queued prompts.              |
| `/approve` | `/approve`        | Approve the pending terminal command awaiting review.                                                  |
| `/deny`    | `/deny`           | Deny the pending terminal command awaiting review.                                                     |
| `/queue`   | `/queue <prompt>` | Queue a prompt for the next turn instead of steering the running turn (plain messages steer by default; unconsumed steers auto-run next). |
| `/compact` | `/compact`        | Summarize this session to markdown; applies to the next reply (auto-fires at 70% context).            |

## CLI Commands

Operator commands run from a terminal against your `.oswald/` root:

| Command                                  | Description                                                                                                     |
| ---------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `oswald setup`                           | Seed missing `config.yaml`/`.env`/`SOUL.md` entries for first install; never overwrites.                        |
| `oswald start`                           | Run the Oswald service in the foreground.                                                                       |
| `oswald profile create\|rename\|delete`   | Provision, rename, or remove a named profile subtree (refuses `default` on delete, requires confirmation).      |
| `oswald mcp list\|add\|edit\|remove\|test` | Manage profile MCP servers (add through staged validation, never prints secrets).                              |
| `oswald mcp auth <server>`               | Interactively authorize an MCP server (browser or device flow).                                                 |
| `oswald gateway show\|set-multiplex`      | Inspect routing and toggle multi-profile routing.                                                               |
| `oswald gateway routes list\|add\|remove` | Manage `gateway.profile_routes` (add is staged-validated).                                                      |
| `oswald platforms show\|enable\|disable`  | Inspect gateway platforms and flip `platforms.<name>.enabled` (service restarts automatically to apply).       |
| `oswald platforms set <platform> [flags]` | Set listener/admission leaves for `discord`, `bluebubbles`, or `api` (admission applies live).                 |
| `oswald providers list\|show\|add\|remove` | Manage LLM providers (`--profile` allowed).                                                                     |
| `oswald model show\|set`                  | Manage model selection: `--provider`, `--default`, `--context-length` (`--profile` allowed).                    |
| `oswald api show\|set-key\|clear-key`      | Manage the per-profile OpenAI gateway credential (`--profile` allowed, never printed).                          |
| `oswald tools show\|set\|unset`            | Manage `web-search`, `image`, `browser`, and `terminal` backends (`--profile` allowed, secrets redacted).      |
| `oswald plugins list\|enable\|disable\|info` | Discover plugins, opt in per profile, and inspect manifest/provenance state (never prints secrets).            |
| `oswald plugins install <source>`        | Install shared plugin code from a git URL, `owner/repo`, or local path (`--ref` pins a full commit SHA, `--subdir` for monorepos, `--enable` with `--profile` to also enable; default scope required). |
| `oswald plugins update <name>`           | Refresh a managed install (pinned installs need an explicit `--ref`; default scope required).                  |
| `oswald plugins remove <name>`           | Drop a plugin for a profile and delete shared code (refuses when other profiles still reference it; `--full` also removes per-profile storage). |
| `oswald plugins install-deps <name>`      | Install a plugin's pinned npm dependencies into the profile home (explicit consent per install).                |
| `oswald plugins bin-path <name>`         | Print the absolute plugin-local binary path for explicit use in `terminal` (PATH is never mutated).            |
| `oswald runtime show\|set`                | Inspect and set `runtime.log_level` (applies live).                                                             |
| `oswald config show\|get\|validate`        | Inspect effective configuration (secrets redacted) and validate files; never writes.                            |
| `oswald env show\|set\|unset`              | Manage the private per-profile `.env` (names only on show, never prints values).                                |

Every mutating group validates through the staged startup pipeline before writing, so a rejected
edit leaves files untouched; each command reports `applied live` vs `restarting service to
apply`. Reads (`show`, `get`, `list`, `validate`) may name another profile; mutations stay
fenced to the calling profile, and global sections plus profile lifecycle require the default
scope — contradictions fail closed instead of landing elsewhere. Log-level and admission
changes apply live; anything else triggers a supervised automatic restart of the service
within seconds — no manual restart, no operator signal.

## Plugins

Plugins extend Oswald with extra tools, hooks, and commands without touching core code.
Code is installed once (shared); enablement, configuration, and storage stay per profile.

A plugin is a directory with a `plugin.yaml` manifest:

```yaml
name: demo
version: "1.0"
description: what it does
manifest_version: 2
api_version: "1"
requires_env: [EXAMPLE_API_KEY]   # fail closed per profile when unset
requires_plugins: [base]          # optional load-order dependencies
node_package: example-cli         # optional pinned npm CLI
node_version: 1.2.3               # exact pin, no ranges
```

```bash
oswald plugins install owner/repo --enable --profile jonah   # or https URL / local path
oswald plugins enable demo --profile jonah
oswald plugins install-deps demo --profile jonah              # pinned npm deps, explicit consent
oswald plugins bin-path demo --profile jonah                 # absolute path for terminal use
```

`install` accepts `--ref <40-char SHA>` to pin (tags and branches are rejected) and
`--subdir` for monorepos; `update` refreshes managed installs (pins never move without an
explicit `--ref`); `remove` refuses when another profile still references the key. Reads
(`list`, `info`, `bin-path`) may name another profile; `enable`/`install-deps` stay fenced
to the calling profile, and code `install`/`update`/`remove` require the default scope.

`terminal` never gains plugin directories on `PATH`: invoke plugin CLIs via the absolute
`bin-path`. API keys live in the profile `.env` (`oswald env set`), never in `plugin.yaml`;
enabled plugins automatically receive their declared `requires_env` values in sandboxed
children (see below), and provider credential names are always refused.

## Sandbox Environment Variables

`terminal`, `execute_code`, and background processes spawn with a fixed minimal environment
plus a deny-by-default passthrough allowlist resolved against the acting profile's own
`.env` files — never another profile's, and never ambient process secrets. Three sources
can register names:

* skill frontmatter: `required_env: [EXAMPLE_API_KEY]` in `SKILL.md` (registered on `skill_view`),
* operator config: `oswald tools set terminal --env-passthrough A,B` (per profile, restart to apply),
* enabled plugins: their manifest `requires_env` (automatic on load).

Well-known credential and identity names (provider keys, bot tokens, `PATH`/`HOME`, `OSWALD_*`)
can never tunnel into a sandbox. Values are resolved per profile at spawn time and are never
logged — list output shows only names and counts.

## Documentation

Everything resides in [`AGENTS.md`](AGENTS.md).

## Inspiration

Oswald takes inspiration from other harness projects like Hermes-Agent, OpenClaw, and OpenCode, so some concepts —
slash commands, tool bridges, layered agent docs — may look familiar. Those projects were
too complex for my needs, carrying functionality I wasn't looking for while missing some
features I wanted. Oswald is the simpler shape I was after, and building it rather
than adopting an existing project gave me a hands-on way to learn about AI — it's a
personal project I run for family and friends.

## License

MIT. See [`LICENSE`](LICENSE).
