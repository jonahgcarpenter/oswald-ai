# workspace/ — Jailed Files, Sandboxed Shell, Tracked Processes

Applies on top of the root `AGENTS.md` (operator-state and never-log-rules).

## Shape

`workspace.go::Workspace` binds one profile's working directory (`default` = profile root, named =
`profiles/<name>/`) and every file/shell operation resolves through `Resolve()` — absolute, `~/`,
and CWD-relative inputs alike. `read_file` / `write_file` / `patch` / `search_files` are the file
tools; `terminal` runs foreground `/bin/sh -c` with a minimal environment
(`TMPDIR=<cwd>/cache/scratch/`); `execute_code` runs stateless `python3` per call with no cross-call
kernel state; `process_manage` polls tracked sessions owned by `process/`; `cronjob_manage` owns
schedules. `guard.go` / `shellscan.go` / `fetchscan.go` implement the pre-execution chain,
`review.go` the LLM reviewer, and `approve_exec.go` the approved replay.

## Containment (the model to hold in your head)

Everything resolves inside the profile CWD, and a denylist fails closed with categorized denies:
application state (`state.db*`, redirecting to `session_search`), credentials (`.env*`,
`credentials.*`, `*credentials.json`, `*.pem`, `*.key` — no `.env.example` oracle),
`mcp-tokens/`, `memories/` (directing to the `memory` tool), `SOUL.md` and operator policy, and
non-scratch `cache/` — plus `profiles/` for the default profile. `../`, absolute-path, and symlink
escapes fail closed, and `profileescape` / `cacheguard` scan shell text (including `cd` targets,
`VAR=cache/..` assignments, and `os.path.join("cache", ...)` literals) so `echo "rm -rf /"` stays
prose while `sh -c "rm -rf /"` stays live.

`write_file` requires a current full `read_file`/`write_file` in the same request and refuses on
external modification (new files allowed; 2 MiB cap; syntax-checked). `search_files` skips
`.git`/`node_modules`/`__pycache__` and files over 1 MiB. Never log workspace paths, contents,
queries, or command output — native history carries 16 KiB args / 16,000 result runes per call
within a 64 KiB aggregate trace.

## Pre-execution chain (order matters)

`terminal` enforces this order, and `force` cannot bypass the lifecycle portion:

1. Lifecycle scan — supervised-gateway markers and referenced scripts (≤1 MiB, NUL-checked;
   unscannable fails closed).
2. Self-repo git-mutation block when a `.git` dir is present.
3. Profile-escape and managed-cache scans → hard deny.
4. Foreground detachment refusal (`nohup`, trailing `&`, dev servers, `http.server`) with a
   `background=true` rewrite hint.
5. Normalized semantic layer (line-continuation/`$IFS`/fullwidth folding, quote-aware positions).

Unrecoverable operations (protected-root wipe, mass kill, power control, DROP/TRUNCATE) deny with
a fixed `status:blocked` envelope that neither the reviewer nor `/approve` can clear. Config
in-place edits (`sed -i`, `tee`, redirections, `cp` over `config.yaml`/`.env*`) deny with an
ask-the-operator message. Safe `https` retrieval to the workspace stays allowed; plain-HTTP off
loopback, credentialed, secret-bearing, out-of-workspace, or executing fetches deny explicitly.
Everything recoverable-looking — permission open-ups, decode-pipes, force-pushes, daemon
redirects, hidden unicode, direct store or secret reads — routes to the background reviewer
(fixed verbatim prompt, temperature 0, no tools, 30s bound, command wrapped as untrusted
`<command>` input, strict one-word parse; errors and unparseables escalate).

`APPROVE` executes inline, `DENY` blocks, `ESCALATE` parks a single-flight pending ask and blocks
the tool call on its decision channel until `/approve` / `/deny` signals it (five-minute window;
expiry, `/new`, `/stop`, and restart finalize as denied; unattended transports fail closed without
parking). The waiter replays the identical invocation (`ExecuteApproved`: lifecycle, self-repo,
sandbox, and the unrecoverable/fetch/config re-checks — skipping only the reviewer step) and the
real output returns as the tool result; denial returns a productive denial tool result so the agent
continues in the same loop. The ask surfaces while blocked as a persistent message outside the turn's lifecycle edits — fenced Discord markdown on streaming transports, a threaded plain-text reply where there is no live stream (iMessage); expiry sends the same kind of follow-up, while /approve and /deny stay silent and let the agent's next reply carry the outcome.

## Processes and cron

`background=true` spawns a tracked session (`session_id`, `persist_on_release`) persisted under
`cache/processes/<id>/`; over-cap foreground runs promote instead of blocking. `process_manage`
(`list|poll|log|wait|kill|write|submit|close|handoff`, prefix match, handoff cap 3) is the only
way to touch them; `write`/`submit` stdin is escape-scanned. Completed entries are retained 24h
after creation, newest 64 per profile, pruned hourly; `StopAll` kills non-persistent entries before
store close.

`cronjob_manage` owns schedules end to end (`create|list|update|pause|resume|remove|run`), rejects
`model/provider/base_url` overrides, and runs with zero-tool `enabled_toolsets:[]`. Scripts get
stdout injection, monitors get diff gates with first-tick baselines, delivery is origin-only.

## Tests

Cover containment denies and their redirect targets, the read-before-write gate, scan
classification (prose vs live), reviewer verdicts and escalation, approval replay through every
re-check, process lifecycle/retention, and cron validation.
