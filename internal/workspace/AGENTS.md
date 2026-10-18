# Workspace - Files, Shell, Processes

`internal/workspace/` owns all local filesystem and shell side-effects. `process/` tracks
background shell processes. Every path resolves inside the profile working directory only.

## Layout

- `workspace.go`, `profileescape.go`, `cacheguard.go` — `Workspace` root binding, `Resolve()`
  containment, scratch dir, cross-profile escape and managed-cache scans.
- `read_file.go`, `write_file.go`, `patch.go`, `search_files.go` — paginated reads, gated writes,
  fuzzy patch, content/filename search.
- `terminal.go`, `execute_code.go` — foreground `/bin/sh -c` with minimal env, stateless `python3`.
- `process_manage.go`, `process/` — tracked sessions (`list|poll|log|wait|kill|write|submit|close|
  handoff`), prefix match, handoff cap 3, 7d/newest-64 retention. Completed kept 24h after
  creation; `StopAll` kills non-persistent entries before store close.
- `cronjob_manage.go` — schedule CRUD plus `run`; self-contained prompts, monitor diff gates,
  origin-only delivery, zero-tool `enabled_toolsets:[]`.
- `guard.go`, `shellscan.go`, `fetchscan.go`, `review.go`, `approve_exec.go` — lifecycle scan,
  self-repo block, semantic layer, fetch classification, LLM reviewer, approved replay.

## Rules

- Containment denylist: state.db, `config.yaml`/`.env*`, credentials (`*.pem`, `*.key`,
  `*credentials.json`), `mcp-tokens/`, `memories/` (use the `memory` tool), `SOUL.md`, non-scratch
  `cache/`, and `profiles/` for the default profile. `../`, absolute, and symlink escapes fail
  closed with categorized denies.
- `write_file` requires a current full `read_file`/`write_file` in the same request and refuses on
  external modification. New files allowed; 2 MiB cap; syntax-checked.
- Pre-execution chain order: lifecycle scan → self-repo git-mutation block → escape scans →
  detach refusal (rewrite with `background=true`) → semantic layer. Unrecoverable operations
  (protected-root wipe, mass kill, power control, DROP/TRUNCATE) deny with a fixed
  `status:blocked` envelope no reviewer or `/approve` can clear. Config in-place edits deny with an
  ask-the-operator message.
- Recoverable patterns route to the background reviewer (fixed prompt, temperature 0, no tools, 30s;
  strict one-word parse). `APPROVE` executes, `DENY` blocks, `ESCALATE` parks a single-flight
  pending ask. `/approve` replays the identical invocation through every check except the reviewer.
- `terminal` runs `TMPDIR=<cwd>/cache/scratch/`; background runs spawn tracked sessions polled via
  `process_manage`. `execute_code` is stateless per call.
- Never log workspace paths, contents, queries, or command output.

## Testing

Cover containment denies and redirects, read-before-write gating, scan classification, reviewer
verdicts, approval replay, process lifecycle/retention, and cron validation.
