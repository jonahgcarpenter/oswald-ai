# Foreign skill → Oswald map

## Detect

- **OpenCode:** `.opencode/skills/<id>/SKILL.md`, grouping dirs (`teams/<id>/`),
  flat `<root>/<id>.md`, `~/.config/opencode/skills`, compat
  `~/.claude/skills`, `~/.agents/skills`, HTTP catalog entries.
- **Hermes:** `skills/<category>/<name>/SKILL.md`, `optional-skills/`,
  plugin-bundled `<plugin>/skills/<name>/`.
- **Claude:** `<plugin>/skills/<name>/SKILL.md`, `~/.claude/skills/`,
  `.claude/skills/`, `.claude/commands/<name>.md` (command files).

## Adapt

- Skill directory name (lowercased) becomes the Oswald skill name; it must
  match `[a-z0-9][a-z0-9_-]{0,63}`. Anything else is refused — rename with
  operator confirmation, never silently.
- Frontmatter kept: `description` (over 60 chars is kept; the index clips it),
  `version` when present, `required_env` when present. Hermes
  `required_environment_variables[].name` maps 1:1 into `required_env`.
- Frontmatter dropped and recorded: `when_to_use` (fold key phrases into the
  description by hand if they matter), `allowed/disallowed-tools`, `model`,
  `effort`, `context`/`agent`, `hooks`, `paths`, `shell`, `argument-hint`,
  `arguments`, `user-invocable`, `disable-model-invocation`, `autoinvoke`,
  `platforms`, `requires_toolsets`/`requires_tools`/`fallback_for_*`,
  `config[]`, `required_credential_files[]`, `blueprint`, `author`, `license`,
  `metadata.*`.
- Nesting is preserved relative to the nearest ancestor `skills` directory
  (or the repo root with leading dot-directories stripped). Oswald derives
  category from the parent dir and resolves same-named skills by relative
  path, so grouping (`teams/review`) survives verbatim.
- Supporting files travel only from `scripts/`, `references/`, `templates/`,
  `assets/`. Everything else is recorded as dropped.
- Flat `<id>.md` files (OpenCode root form, Claude command files) convert to
  `skills/<id>/SKILL.md` with the file body verbatim; the Oswald name falls
  back to the directory name and the description to the first body line.

## Always dropped (recorded, never emulated)

Tool handlers, hooks, slash/CLI commands, MCP servers, permissions,
model/effort overrides, subagent execution, template vars (`${...}`),
inline shell (`` !`cmd` ``), dynamic context (`@file`), blueprints/cron,
credential mounts, marketplace/versioning/auto-update, managed settings.
Copy the markdown; leave the platform behind.
