# `oswald setup`

Seed a first-install configuration root. Missing entries only — never
overwrites, and rolls back partial state on failure.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `setup` | `usage: oswald setup` | seeds `.oswald/` (global `config.yaml`, private `.env`, `SOUL.md`, `memories/`, `cache/`, `vault/`, `state.db`) |

## Notes

- Run once per machine (or per `--root DIR`). Re-running is safe: existing
  files are left untouched.
- Takes no flags and no `--profile`: it provisions the default scope, under
  which named profiles are later created with `oswald profile create`.
- After setup, verify with `oswald config validate` and
  `oswald providers status`.

## Failures

- Partial failure rolls back: no half-written root is left behind. Re-run
  `oswald setup` after fixing the cause (permissions, disk space).
