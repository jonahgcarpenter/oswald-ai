# Workspace - Files, Shell, Processes

`internal/workspace/` owns all local filesystem and shell side-effects. `process/` tracks
background shell processes. Every path resolves inside the profile working directory only.

## Packages

- `workspace` (root) — `Workspace` root binding and `Resolve()` containment; `read_file`,
  `write_file` (read-before-write gate), `patch`, `search_files`; `terminal` (foreground `/bin/sh
  -c` plus tracked background spawn) and stateless `execute_code`; `process_manage` companion;
  `cronjob_manage` schedules; `guard`/`shellscan`/`fetchscan` pre-execution chain plus the LLM
  `review` step and `approve_exec` replay; `profileescape`/`cacheguard` escape scans; `scratch`
  and `prunehook` cache sweeps.
- `process` (`process/`) — per-profile tracked process manager: spawn, prefix resolution, log
  tailing, wait/kill/write/submit/close, capped handoff, 24h/newest-64 pruning, `StopAll`.

## Rules

- Containment denylist: state.db, `config.yaml`/`.env*`, credentials (`*.pem`, `*.key`,
  `*credentials.json`), `mcp-tokens/`, `memories/` (use the `memory` tool), `SOUL.md`, non-scratch
  `cache/`, and `profiles/` for the default profile. `../`, absolute, and symlink escapes fail
  closed with categorized denies.
- `write_file` requires a current full `read_file`/`write_file` in the same request and refuses on
  external modification. New files allowed; 2 MiB cap; syntax-checked.
- Chain order: lifecycle scan → self-repo git-mutation block → escape scans → detach refusal
  (rewrite with `background=true`) → semantic layer. Unrecoverable operations (protected-root wipe,
  mass kill, power control, DROP/TRUNCATE) deny with a fixed `status:blocked` envelope no reviewer
  or `/approve` can clear. Config in-place edits deny with an ask-the-operator message.
- Recoverable patterns route to the background reviewer (fixed prompt, temperature 0, no tools,
  30s; strict one-word parse). `APPROVE` executes, `DENY` blocks, `ESCALATE` parks a single-flight
  pending ask. `/approve` replays the identical invocation through every check except the reviewer.
- `terminal` runs with `TMPDIR=<cwd>/cache/scratch/`; over-cap foreground runs promote to tracked
  background sessions. `execute_code` is stateless per call.
- Never log workspace paths, contents, queries, or command output.

## Testing

Cover containment denies and redirects, read-before-write gating, scan classification, reviewer
verdicts, approval replay, process lifecycle/retention, and cron validation.
