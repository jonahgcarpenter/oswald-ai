---
name: code-review
description: Review code changes for correctness and missing tests
version: 1.0.0
---

# Code Review

Review the current changes for correctness, missing tests, and style drift.

## Procedure

1. Read the diff or the files the operator names.
2. Check every behavior change for a covering test.
3. Flag risks using `references/checklist.md`.
4. Report findings grouped by must-fix vs suggestion.
