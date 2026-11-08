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

## Operator workflow (install and setup)

Code is installed once (shared `plugins-available/`); enablement, config, and storage
stay per profile. From a local shell (default scope):

```bash
oswald plugins install owner/repo --enable --profile jonah  # https URL or local path too
oswald plugins enable demo --profile jonah                   # opt in later / re-enable
oswald plugins install-deps demo --profile jonah             # pinned npm deps, explicit consent
oswald plugins bin-path demo --profile jonah                 # absolute path for terminal use
oswald plugins update demo [--ref <40-char SHA>]             # pins never move without --ref
oswald plugins remove demo --profile jonah [--full]          # refuses when other profiles reference it
```

`install` accepts `--ref` (full SHA only; tags/branches rejected) and `--subdir` for
monorepos; unpinned installs follow the remote HEAD on `update`. `--profile` selects
the enable target under the normal fencing (mismatched `OSWALD_HOME` fails closed);
code `install`/`update`/`remove` require the default scope. Reads (`list`, `info`,
`bin-path`) may name another profile. Everything plugins-related is restart-classified.

`plugin.yaml` shape (v2 contract, unknown keys rejected):

```yaml
name: demo              # 2–40 lowercase ASCII, `soul` reserved; the canonical key
version: "1.0"
description: what it does
manifest_version: 2
api_version: "1"
requires_env: [EXAMPLE_API_KEY]  # fail closed per profile when unset
requires_plugins: [base]         # optional load-order dependencies
node_package: example-cli        # optional pinned npm CLI
node_version: 1.2.3              # exact pin, no ranges
```

API keys live in the profile `.env` (`oswald env set`), never in `plugin.yaml`.
Enabled plugins automatically receive their declared `requires_env` values in sandboxed
children (terminal, execute_code, background processes) from the acting profile's scope;
see `internal/workspace/AGENTS.md` for the passthrough contract. Provider credential names
are always refused. `terminal` never gains plugin directories on `PATH` — invoke plugin
CLIs via the absolute `bin-path`.

## Tests

Synthetic temp dirs only, never live profiles. Cover manifest validation, precedence and
impostor rejection, enable/disable gate, override gate, hook order and timeout, storage
confinement — not rendered prompts.
