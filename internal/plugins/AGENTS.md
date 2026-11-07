# plugins/ — Profile-Scoped Plugin Host

Applies on top of the root `AGENTS.md` (operator-state, persisted-contract, and never-log rules).

## Shape

One `Manager` per profile (`scope_key` is the absolute profile root). Shared code,
per-profile enable/config/storage — same island rule as `mcp_servers`:

- `types.go` owns `Manifest`, hook names, `Registration`, `PluginContext`, typed errors.
- `manifest.go` parses `plugin.yaml` (name 2–40 lc ASCII, `soul` reserved; version,
  entry, `requires_env`, `capabilities`).
- `discovery.go` scans bundled → user → opt-in project dirs; later wins on key
  collision with impostor-dir rejection.
- `manager.go` tracks loaded plugins, tool/hook/command/skill registrations, and
  per-profile `plugins/<name>/` storage (`0700` dirs, `0600` files, workspace-protected).
- `runtime.go` repairs `plugins/` roots, loads enabled plugins from shared
  `plugins-available/` + opt-in project dirs, checks `requires_env` fail-closed,
  and publishes tools into the profile registry with override gating.
- `agent` fires `pre_tool_call` (directive, 5s, fail-closed `plugin_blocked`),
  `post_tool_call` (observer, 30s, fail-open), and `session_start/end` observers.

## Contracts

- Discovery finds code; `plugins.enabled` (per-profile `config.yaml`) loads it. Nothing
  with hooks or tools runs until enabled. `plugins.disabled` always wins.
- Tool override of a builtin needs `plugins.entries.<id>.allow_tool_override: true`
  (or `tools.override` capability), else fail closed with `PluginToolOverrideError`.
- Hooks run in registration order on a shared mutable event; `pre_tool_call` timeouts
  fail closed (block), observers fail open. No hook logs prompts, args, URLs, or secrets —
  only counts and names via allowlisted `config.F` fields.
- `Storage()` is rooted at `<profileRoot>/plugins/<name>/`; no direct `state.db`/`.env`
  handles cross profiles. Registry restores are identity-conditional so one profile's
  unload never clears another's.

## Tests

Synthetic temp dirs only, never live profiles. Cover manifest validation, precedence and
impostor rejection, enable/disable gate, override gate, hook order and timeout, storage
confinement — not rendered prompts.
