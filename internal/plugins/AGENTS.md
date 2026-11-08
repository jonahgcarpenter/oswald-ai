# plugins/ — Profile-Scoped Plugin Host

Applies on top of the root `AGENTS.md` (operator-state, persisted-contract, and never-log rules).

## Shape

One `Manager` per profile (`scope_key` is the absolute profile root). Shared code,
per-profile enable/config/storage — same island rule as `mcp_servers`:

- `types.go` owns `Manifest` (including `manifest_version: 2`, `api_version: "1"`,
  `requires_plugins`), hook names, `Registration`, `PluginContext`, typed errors.
- `manifest.go` parses `plugin.yaml` (name 2–40 lc ASCII, `soul` reserved; version,
  entry, `requires_env`, `capabilities`, optional pinned `node_package@node_version`
  exact x.y.z plus `node_bin`). Unknown keys, ranges, and contract mismatches fail
  closed. Keys are canonical and exact: no bare-name aliases anywhere.
- `discovery.go` scans bundled → user → opt-in project dirs; later wins on key
  collision with impostor-dir rejection; `ResolveLoadOrder` topologically orders
  `requires_plugins` (unknown/cycle fails closed).
- `install.go` owns URL installs (`owner/repo`, https git, local paths; ssh refused)
  with full-SHA pins, `--subdir`, `--force` backup to `plugins-backup/`, and
  `.install-metadata.json` provenance (source/url/revision, never secrets).
  `UpdateFromURL` refreshes managed git installs (pins need explicit `--ref`);
  `RemoveInstall` deletes shared code after callers verify no other profile
  references the key. Ranges, tags, URLs, and file specs fail closed.
- `node.go` owns per-profile Node homes (`<profileRoot>/plugins/<name>/`): pinned
  `npm install` with bounded output and timeouts (`install-deps` is the consent
  gate), plus `NodeBinPath` absolute resolution. The terminal `PATH` stays fixed;
  callers invoke the absolute path explicitly.
- `discovery.go` scans bundled → user → opt-in project dirs; later wins on key
  collision with impostor-dir rejection.
- `manager.go` tracks loaded plugins, tool/hook/command/skill registrations, and
  per-profile `plugins/<name>/` storage (`0700` dirs, `0600` files, workspace-protected).
  `EnabledRequiresEnv` reports each enabled plugin's declared `requires_env` so
  startup can register them for sandbox passthrough.
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
