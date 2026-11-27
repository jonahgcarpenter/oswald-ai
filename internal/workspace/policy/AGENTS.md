# workspace/policy/ — Pre-Execution Security Chain

Applies on top of the root `AGENTS.md` and `internal/workspace/AGENTS.md`
(never-log rules: never log commands, paths, contents, or env values).

## Shape

Pure functions only; this package never imports `internal/workspace`
(parent imports child, never the reverse). `groups.go` owns the file-group
classifier (`Group`, `Classify`, `DenyError`, `DenyMessage`); `guard.go` owns
lifecycle/self-repo/detach/danger scans; `shellscan.go` owns the normalized
semantic layer; `fetchscan.go` owns curl/wget verdicts (`ClassifyFetch`,
`FetchDeny/Allow/Skip`); `confinewrite.go` owns write-sink and protected-read
scans; `envscan.go` owns `OSWALD_*` retarget scans. `policy_alias.go` in the
parent re-exports `Group`, `Classify`, `Scan*`, and `ValidateWorkdirString`
so existing `workspace.*` callers keep working.

## Order matters

`command.go::ExecuteCommand` and `approve_exec.go::ExecuteApproved` call in
this order: lifecycle → self-repo → confined-write → protected-read →
env-retarget → detach refusal → normalized semantic (unrecoverable, fetch,
store/secret, hidden-unicode, recoverable, dangerous). `force` never bypasses
the lifecycle portion. Approved replay re-runs every deny-class rule except
the reviewer step.

## Bounds that bite

Fail closed with rule names only. Reads roam (secret reads escalate);
writes deny outside agent-owned groups with the group message. `~`/`$HOME`
classify against the acting profile root.

## Tests

Cover the file-group matrix, write-sink classification (prose vs live,
shell vs Python), env retargeting, reviewer verdicts, and replay re-checks —
with synthetic commands, never live paths.
