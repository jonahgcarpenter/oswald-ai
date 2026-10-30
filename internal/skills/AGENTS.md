# skills/ — Profile Skill Catalog, Index, On-Demand Reads

Applies on top of the root `AGENTS.md` (operator-state and never-log-rules).

## Shape

One root feeds one catalog: the profile-managed `<profileRoot>/skills/` tree, scanned
fresh on every call — no cache, so tool edits are visible on the next turn with
nothing to invalidate. `frontmatter.go` parses the `SKILL.md` subset (`name`,
`description`, `version`, `platforms`, `prerequisites`, tags; unknown keys ignored),
`scan.go` walks it (dot-dirs skipped, directory symlinks unfollowed, files over
1 MiB skipped, malformed/unclosed fences skipped — a scan never fails for one file),
`catalog.go` resolves it, `index.go` renders the prompt block, `read.go` loads
bodies, `store.go` binds roots and owns create/update. The model-facing tool
adapter lives in `internal/tools/skill_manage/`; this package never imports it.

## Catalog contract

`resolveCatalog` filters by the `platforms` gate (empty admits everything, otherwise
exact case-insensitive gateway match), then settles each declared name: unique names
load by name, same-named copies load by exact path relative to the skills root
(`a/one`), and byte repeats in one directory collapse. Display category is the
parent directory relative to the root (`general` at top level); output sorts by
category then load name so the prompt is byte-stable. No disabled list, no external
tier: everything listed is loadable.

## Budgets (review every change against these)

Descriptions clip to 200 runes, the rendered `## Skills` index caps at 6,000 runes
(head coaching preserved, rows trimmed tail-first, `""` when empty), bodies and
supporting files cap at 16,000 runes with a truncation marker, at most 64
supporting files listed from `references/`, `templates/`, `scripts/`, `assets/`.
`View` matches load name, then declared name (exact, then case-insensitive), then
directory base name; shared names error with the disambiguating paths instead of
guessing. `file_path` reads stay inside the skill directory (symlink-resolved);
`..`, absolute paths, and escapes fail closed. `Create` enforces the
`[a-z0-9][a-z0-9_-]{0,63}` slug, `0700` dirs, `0600` files, and refuses existing
names; `Update` needs an exact unique `old_text` match and keeps file permissions.

## Tests

Synthetic temp dirs only, never live profiles. Cover frontmatter fallbacks (dir-name
name, first-body-line description), scan skips, precedence/relpath/platform/category
resolution, index bounds and coaching presence, view matching/ambiguity/confinement,
and create/update validation — not rendered-prompt snapshots.
