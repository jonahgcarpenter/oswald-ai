# Claude bundle → Oswald map

## Detect

- `.claude-plugin/plugin.json` (+ optional `marketplace.json`), and/or
- skill dirs: `<plugin>/skills/<skill-name>/SKILL.md`, `~/.claude/skills/`,
  `.claude/skills/`, nested `<subdir>/.claude/skills/`, or `.claude/commands/`
  (older command-file format, still valid).

## Skills in

- One skill per `<skill-name>/` directory with `SKILL.md`; command files are
  bare `<name>.md`.
- Frontmatter: `name` (command name, defaults to directory), `description`
  (+ `when_to_use`, truncated together at 1,536 chars in listings),
  `disable-model-invocation`, `user-invocable`, `allowed-tools`,
  `disallowed-tools`, `model`, `effort`, `context: fork`, `agent`, `hooks`,
  `paths`, `shell`, `argument-hint`, `arguments`, `license`, `compatibility`.

## Skills out

- Skill dirs → `skills/<skill-name>/SKILL.md` (relative nesting preserved).
- Command files → `skills/<name>/SKILL.md` (content verbatim).
- Frontmatter kept: `name` (= directory name), `description`, `version` when
  present. Dropped and recorded: `when_to_use` (fold its key phrases into the
  description by hand if they matter), `disable-model-invocation`,
  `user-invocable`, `allowed-tools`/`disallowed-tools`, `model`/`effort`,
  `context: fork`/`agent`/`background`, `hooks`, `paths`, `shell`,
  `argument-hint`/`arguments`, dynamic context (`!cmd`, `@file`, `${vars}`),
  `license`/`compatibility`.
- Plugin skills stay namespaced: Oswald loads them as `<plugin>/<skill>`,
  mirroring Claude's `/plugin-name:skill-name`.

## Always dropped (recorded, never emulated)

Marketplace installs/versions/auto-update, MCP bundles (`.mcpb`/`.dxt`),
`extraKnownMarketplaces`, managed/org settings, background monitors, session
lifecycle rules. Copy the markdown; leave the platform behind.
