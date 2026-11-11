# Hermes → Oswald map

## Detect

- `plugin.yaml` (`name`, `version`, `description`, `provides_tools`,
  `provides_hooks`, `requires_env`, `capabilities`) plus
  `__init__.py` with `register(ctx)`, and/or
- skill trees: `skills/<category>/<name>/SKILL.md`, `optional-skills/`, or
  plugin-bundled `<plugin>/skills/<name>/`.

## Skills in

- `skills/<category>/<name>/SKILL.md` (+ `scripts/`, `references/`).
- Frontmatter: `name`, `description`, `version`, `author`, `license`,
  `platforms: [macos|linux|windows]`, `metadata.hermes.{tags, related_skills,
requires_toolsets, requires_tools, fallback_for_toolsets, fallback_for_tools,
config[], blueprint}`, `required_environment_variables[]` (each with
  `name`, `prompt`, `help`, `required_for`), `required_credential_files[]`.

## Skills out

- Preserved nesting: `skills/<category>/<name>/SKILL.md` (Oswald category =
  parent dir, same shape).
- Frontmatter kept: `name` (= directory name), `description`, `version`.
- `required_environment_variables[].name` → `required_env: [names]`
  (prompts/help/required_for are dropped; secrecy lives in `.env` either way).
- Emitted manifest keeps `requires_env` empty: env stays per-skill so one
  missing key never blocks the plugin. Skill views still register passthrough.
- Dropped and recorded: OS `platforms` (noted; Oswald has no OS gate),
  `requires_toolsets`/`requires_tools`/`fallback_for_*` (no toolset hiding),
  `config[]` (no non-secret config injection), `required_credential_files[]`
  (no credential mounts), `blueprint:` (no cron mapping — use
  `cronjob_manage` explicitly), template vars (`${HERMES_SKILL_DIR}`,
  `${HERMES_SESSION_ID}`), inline `` !`cmd` `` snippets, `author`/`license`
  (kept only if you copy them into the body yourself).

## Always dropped (recorded, never emulated)

Python `register()` handlers (`register_tool`, `register_hook`,
`register_command`, `register_cli_command`), slash commands, platform/image-gen/
delegation/memory/context-engine/model-provider subkinds, pip entry-points,
`dispatch_tool`, host LLM calls, capability grants, approval transports,
`plugins.enabled/disabled` semantics (Oswald uses presence).
