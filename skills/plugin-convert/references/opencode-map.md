# OpenCode → Oswald map

## Detect

- `package.json` with an `@opencode/plugin` dependency and `exports` for
  `./server`, `./tui`, `./gui-experimental`, or `./rpc`, and/or
- skill dirs: `.opencode/skills/`, `~/.config/opencode/skills`, compat
  `.claude/skills`, `.agents/skills`, or explicit `skills[]` entries / HTTP
  catalogs (`index.json`).

## Skills in

- Directory form: `<root>/<grouping...>/<id>/SKILL.md` (one skill per
  directory; grouping dirs are not skills).
- Flat form: `<root>/<id>.md` (no supporting files).
- ID is path-derived (directory or file basename, case-sensitive); frontmatter
  `name` is display-only.

## Skills out

- Directory form → `skills/<relative/path>/<id>/SKILL.md` (relative nesting
  preserved; Oswald derives category from the parent dir and resolves
  same-named skills by relative path).
- Flat form → `skills/<id>/SKILL.md` (content copied verbatim; Oswald falls
  back to directory name and first body line).
- Frontmatter kept: `description` (index use). `name` is reset to the Oswald
  skill directory name; `version` kept when present. Dropped and recorded:
  `metadata.opencode/autoinvoke`, `disable-model-invocation`, `license`,
  `compatibility`, HTTP catalog metadata.
- Supporting files: only `scripts/`, `references/`, `templates/`, `assets/`
  travel. Everything else is recorded as dropped.

## Always dropped (recorded, never emulated)

Server/TUI/GUI/RPC parts (`Plugin.define`, `ctx.*.transform()` for tools,
skills, commands, agents, models, providers, MCP, permissions, sessions,
events, hooks, storage), skill `permissions`, HTTP catalogs (`index.json`
refresh semantics), `options` objects.
