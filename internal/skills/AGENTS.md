# skills/ — Profile Skill Catalog, Index, On-Demand Reads

Applies on top of the root `AGENTS.md` (operator-state and never-log-rules).

## Shape

One root feeds one catalog: the profile-managed `<profileRoot>/skills/` tree, scanned
fresh on every call — no cache, so tool edits are visible on the next turn with
nothing to invalidate. `frontmatter.go` parses the frozen `SKILL.md` subset (`name`,
`description`, `version`, `required_env`; unknown keys ignored — including
`platforms`, `prerequisites`, `tags`, `metadata`, `when_to_use`, `config`,
`blueprint`, and any foreign-harness fields), `scan.go` walks it (dot-dirs skipped, directory symlinks unfollowed, files over
1 MiB skipped, malformed/unclosed fences skipped — a scan never fails for one file),
`catalog.go` resolves it, `index.go` renders the prompt block, `read.go` loads
bodies, `store.go` binds roots and owns create/update. `hub.go` owns the install
surface: URL parsing (git shorthand/URLs, local paths), quarantine staging with
manifest validation, atomic publish, and the per-profile `skills.lock.json`
provenance sidecar. The model-facing tool
adapter lives in `internal/tools/skill_manage/`; this package never imports it.

## Catalog contract

`resolveCatalog` performs no platform gating (the platform argument is accepted
but ignored: every skill is visible on every gateway), then settles each declared name: unique names
load by name, same-named copies load by exact path relative to the skills root
(`a/one`), and byte repeats in one directory collapse. Display category is the
parent directory relative to the root (`general` at top level); output sorts by
category then load name so the prompt is byte-stable. No disabled list, no external
tier: everything listed is loadable.

## Budgets (review every change against these)

Descriptions clip to 60 characters, the rendered `## Skills` index caps at 6,000 runes
(head coaching preserved, rows trimmed tail-first, `""` when empty), bodies and
supporting files cap at 16,000 runes with a truncation marker, at most 64
supporting files listed from `references/`, `templates/`, `scripts/`, `assets/`.
`View` matches load name, then declared name (exact, then case-insensitive), then
directory base name; shared names error with the disambiguating paths instead of
guessing. `file_path` reads stay inside the skill directory (symlink-resolved);
`..`, absolute paths, and escapes fail closed. A viewed skill's `required_env`
names register for sandbox passthrough (provider credential names refused).
`Create` enforces the
`[a-z0-9][a-z0-9_-]{0,63}` slug, `0700` dirs, `0600` files, and refuses existing
names; `Update` needs an exact unique `old_text` match and keeps file permissions.

## Tests

Synthetic temp dirs only, never live profiles. Cover frontmatter fallbacks (dir-name
name, first-body-line description) and unknown-key tolerance, scan skips, relpath/category
resolution, index bounds and coaching presence, view matching/ambiguity/confinement,
and create/update validation — not rendered-prompt snapshots.
