# Oswald plugin library (seed)

Starter content for the operator-maintained plugin library. Intended to move
to its own repository; then each entry gains a reviewed full-SHA pin and
installs as `oswald plugins install owner/repo --subdir plugins/<name>
[--ref <sha>] [--profile <name>]`.

## Layout

- `plugins/<name>/` — one installable plugin source (`plugin.yaml` + `skills/`
  [+ pinned `package.json` lockfile when a sidecar needs more than one package]).
- `catalog.json` — name, description, and source path per plugin.

## Seeds

- `hello-skills` — skills-only, no sidecar, no env. Installs anywhere.
- `cli-helper` — declares the pinned `node_package` sidecar convention
  (`prettier@3.1.0`); install pulls the sidecar unless `--no-deps` is passed.
- `env-gated` — `requires_env: [SYNTH_LIBRARY_TOKEN]`; the plugin loads only
  on profiles where that variable is set, proving per-profile fail-closed
  gating.

Presence means enabled: installing into a profile enables it there. Updates
replace `code/` only; `data/home` logins survive. Foreign plugins are never
added here directly — convert them first with the `plugin-convert` skill.
