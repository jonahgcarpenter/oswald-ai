# plugins/ — Profile-Scoped Plugin Host

Applies on top of the root `AGENTS.md` (operator-state, persisted-contract, and never-log rules).

## Frozen (do not extend without operator approval)

The adapter is minimal and frozen: manifest keys are exactly `name, version,
description, entry, requires_env, capabilities, allow_tool_override,
node_package, node_version, node_bin, manifest_version, api_version,
requires_plugins, kind` (unknown keys reject); skill frontmatter keys are exactly
`name, description, version, required_env` (unknown keys ignored); layout is
exactly `<profile>/plugins/<name>/{code,data}` with presence meaning enabled.
Explicitly unsupported, now and by design: autoinvoke/visibility flags, skill
permissions, per-skill tool/model scoping, subagent fork, template variables,
inline shell, blueprints/cron mapping, credential-file mounts, non-secret config
maps, HTTP catalogs, compat-dir search, options objects, and any TS/Python
runtime hosting. Foreign plugins needing those are converted markdown-only with
the gap reported — the adapter does not grow to absorb them.

## Shape

One `Manager` per profile (`scope_key` is the absolute profile root). Code,
config, and storage all live under the profile — same island rule as `mcp_servers`:

- `types.go` owns `Manifest` (including `manifest_version` 1 or 2, `api_version: "1"`,
  `requires_plugins`), hook names, `Registration`, `PluginContext`, typed errors.
- `manifest.go` parses `plugin.yaml` (name 2–40 lc ASCII, `soul` reserved; version,
  entry, `requires_env`, `capabilities`, optional pinned `node_package@node_version`
  exact x.y.z plus `node_bin`). Unknown keys, ranges, and contract mismatches fail
  closed. Keys are flat manifest names, canonical and exact.
- `discovery.go` scans `<profileRoot>/plugins/*/code/plugin.yaml`; directory must
  match the manifest name (mismatches never load); `ResolveLoadOrder`
  topologically orders `requires_plugins` (unknown/cycle fails closed). Presence
  means enabled; there is no shared, bundled, or project discovery.
- `install.go` owns per-profile URL installs (`owner/repo`, https git, local
  paths; ssh refused) into `<profile>/plugins/<name>/code` with full-SHA pins,
  `--subdir`, `--force` backup of code only to `<profile>/plugins-backup/`, and
  `.install-metadata.json` provenance (source/url/revision, never secrets).
  `UpdateFromURL` refreshes managed git installs (pins need explicit `--ref`,
  `data/` untouched); `RemoveInstall` deletes code by default, everything with
  `full`. Ranges, tags, URLs, and file specs fail closed.
- `node.go` owns the per-profile Node sidecar (`<profile>/plugins/<name>/code/node_modules`,
  installed via `install-deps` as the consent gate, profile-fenced): pinned
  `npm install` with bounded output and timeouts, plus `NodeBinPath` absolute
  resolution. npm cache/tmp live under `data/cache/node` so `code/` stays a clean
  copy. The terminal `PATH` stays fixed (plus the server-resolved node dir so
  shims run); callers invoke the absolute path explicitly.
- `exec.go` runs one profile-installed plugin binary (`ExecPluginBinary`):
  fixed base + profile passthrough, `HOME` redirected to `data/home` and
  `cwd`/`TMPDIR` to `data/` so CLI config files land on the persisted volume,
  stdin closed, bounded output. Presence means enabled.
- `manager.go` tracks loaded plugins, tool/hook/command/skill registrations, and
  per-profile `plugins/<name>/data` storage (`0700` dirs, `0600` files,
  workspace-protected). `EnabledRequiresEnv` reports each present plugin's
  declared `requires_env` so startup can register them for sandbox passthrough.
- `runtime.go` repairs `plugins/` roots, loads every present plugin from the
  profile tree, checks `requires_env` fail-closed, and publishes tools into the
  profile registry with override gating.
- `agent` fires `pre_tool_call` (directive, 5s, fail-closed `plugin_blocked`),
  `post_tool_call` (observer, 30s, fail-open), and `session_start/end` observers.

## Contracts

- Discovery finds code; presence loads it. Nothing with hooks or tools runs
  unless its directory exists under the acting profile. Missing `requires_env`
  fails closed for that plugin only.
- Tool override of a builtin needs `plugins.entries.<id>.allow_tool_override: true`
  (or `tools.override` capability), else fail closed with `PluginToolOverrideError`.
- Hooks run in registration order on a shared mutable event; `pre_tool_call` timeouts
  fail closed (block), observers fail open. No hook logs prompts, args, URLs, or secrets —
  only counts and names via allowlisted `config.F` fields.
- `Storage()` is rooted at `<profileRoot>/plugins/<name>/data`; code lives in the
  sibling `code/` tree. `update`/`install --force` never touch `data/`; only
  `remove --full` deletes it. No direct `state.db`/`.env` handles cross profiles.
  Registry restores are identity-conditional so one profile's unload never clears
  another's.

## Operator workflow (install and setup)

Code, config, and storage stay per profile. From the calling profile's scope
(`--profile` contradictions fail closed):

```bash
oswald plugins install owner/repo --profile jonah  # https URL or local path too
oswald plugins install owner/repo --node-package agent-data --node-version 0.13.0 --profile jonah  # sidecar for manifests predating node_package
oswald plugins install-deps demo --profile jonah   # pinned npm deps, per-profile, explicit consent
oswald plugins bin-path demo --profile jonah       # absolute path for terminal use
oswald plugins exec demo -- --version --profile jonah  # run for the calling profile (HOME=data/home)
oswald plugins update demo [--ref <40-char SHA>] --profile jonah  # pins never move without --ref
oswald plugins remove demo --profile jonah [--full]  # code only; --full also deletes data/
```

`install` accepts `--ref` (full SHA only; tags/branches rejected) and `--subdir` for
monorepos; unpinned installs follow the remote HEAD on `update`. Manifests at
`manifest_version` 1 or 2 load (`api_version` defaults to `"1"` when absent;
`kind` must be empty or `standalone`). The node sidecar installs as part of
`install` unless `--no-deps` opts out, from the manifest declaration, explicit
`--node-package`/`--node-version` flags (both required together; conflicts with a
manifest declaration fail closed), or a root `package.json` with a lockfile
(`npm ci`; a `package.json` without one is ignored). The resolved spec is
recorded in `code/.install-metadata.json`, so `install-deps`, `bin-path`, and `exec`
fall back to it when the manifest declares nothing; `update` carries it forward
and refreshes the sidecar after moving code. Reinstalling the same source is a
no-op that repairs a missing sidecar. Reads (`list`, `info`, `bin-path`) may name
another profile; mutations stay fenced to the calling scope
(`OSWALD_HOME` mismatch fails closed). Everything plugins-related is restart-classified.
Old shared `plugins-available/` trees are orphaned and never read; operators may
delete them by hand. Foreign ecosystems (OpenCode, Hermes, Claude bundles) are
not installed directly: convert first with the `plugin-convert` skill
(`skills/plugin-convert/`, markdown-only, runtimes never transfer), then
`oswald plugins install` the staged source. The operator-maintained library
seed lives in `library/` (`catalog.json` + `plugins/<name>/`, intended to move
to its own repository with reviewed pins).

`plugin.yaml` shape (v1 or v2 contract, unknown keys rejected):

```yaml
name: demo              # 2–40 lowercase ASCII, `soul` reserved; the canonical key
version: "1.0"
description: what it does
manifest_version: 2   # 1 or 2 accepted
api_version: "1"      # optional; defaults to "1"
kind: standalone      # optional; empty or standalone only
requires_env: [EXAMPLE_API_KEY]  # fail closed per profile when unset
requires_plugins: [base]         # optional load-order dependencies
node_package: example-cli        # optional pinned npm CLI
node_version: 1.2.3              # exact pin, no ranges
```

API keys live in the profile `.env` (`oswald env set`), never in `plugin.yaml`.
Present plugins automatically receive their declared `requires_env` values in sandboxed
children (terminal, execute_code, background processes) from the acting profile's scope;
see `internal/workspace/AGENTS.md` for the passthrough contract. Provider credential names
are always refused. `terminal` never gains plugin directories on `PATH` — invoke plugin
CLIs via the absolute `bin-path`, or `oswald plugins exec` (same sandbox env plus `HOME`
redirected to `data/home` so CLI config files persist on the volume).

## Tests

Synthetic temp dirs only, never live profiles. Cover manifest validation, profile
scan and directory-name matching, requires_env gate, data preservation on
update/remove, override gate, hook order and timeout, storage confinement — not
rendered prompts.
