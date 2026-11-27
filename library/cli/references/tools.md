# `oswald tools`

Manage tool backends (`tools.*`): web search, image generation, headless
browser, and terminal. Reads take `--profile`; writes fence to the calling
scope.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `show` | `usage: oswald tools show [web-search\|image\|browser\|terminal] [--profile NAME]` | one backend or the whole section, secrets redacted |
| `set` | `usage: oswald tools set <web-search\|image\|browser\|terminal> [flags] [--profile NAME]` | backend leaves (below) |
| `unset` | `usage: oswald tools unset <backend.field> [--profile NAME]` | clears one leaf, e.g. `web-search.brave-key` |

## `set` flags

- `web-search`: `--brave-key REF` (literal or `${VAR}`), `--searxng-url URL`.
- `image`: `--url URL` (ComfyUI base), `--generation-timeout DUR` (e.g. `2m`).
- `browser`: `--launch-command CMD`, `--endpoint-url URL`,
  `--command-timeout DUR`, `--snapshot-threshold N`,
  `--inactivity-timeout DUR`, `--record-sessions true|false`,
  `--allow-private-urls true|false`, `--allow-insecure-urls true|false`,
  `--restrict-evaluate true|false`.
- `terminal`: `--env-passthrough A,B` (comma-separated variable names
  forwarded into sandboxed children).

## Effects

Backend changes are **restart-class** (tool backends are load-bearing).
`unset` restores the built-in default for that leaf.

## Examples

```
oswald tools show web-search
oswald tools set web-search --brave-key '${BRAVE_KEY}'
oswald tools set image --url http://127.0.0.1:8188 --generation-timeout 2m
oswald tools set terminal --env-passthrough PATH,HOME
oswald tools unset web-search.brave-key
```

## Failures

- Unknown backend/field, bad URL or duration, or non-boolean where
  `true|false` is required: usage/exit-1, disk unchanged. Run
  `<command> --help` for the authoritative flag list.
