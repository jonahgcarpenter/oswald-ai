---
name: Bug report
about: Something broken in Oswald AI
labels: bug
---

<!-- Fill each section. An issue without Evidence (repro + exact line) will
     go back for questions before any fix is attempted.
     File with: gh issue create --template bug_report.md --label bug --body-file <file> --title "..." -->

## Claim

<!-- One sentence: what is broken. -->

## Evidence

<!-- Minimal repro steps from a clean state, verified on the current base.
     Include the exact file and line where the behavior manifests
     (e.g. internal/agent/agent.go:412) and the commit you reproduced on. -->

- Commit:
- Repro:
- Exact line:

## Area

<!-- One of: app, agent, broker, session, tools, workspace, transport,
     providers, mcp, platform. Delete the rest. -->

## Verification

<!-- Tagged commands run, e.g. go test -tags sqlite_fts5 ./internal/agent/.
     State what passed and what failed. -->

## Secrets check

<!-- Confirm: no tokens, credentials, prompts, message contents, URLs,
     or raw errors are included anywhere above. -->
