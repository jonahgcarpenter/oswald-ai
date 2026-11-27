# `oswald skills`

Manage per-profile skill installs. Reads take `--profile`; writes fence to
the calling scope. (This CLI manages skills; the `skill_manage` *tool* is
what the agent uses mid-turn to view/create/update them.)

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `list` | `usage: oswald skills list [--profile NAME]` | installed skills |
| `info` | `usage: oswald skills info <name> [--profile NAME]` | manifest + provenance sidecar |
| `install` | `usage: oswald skills install <source> [--ref SHA] [--subdir PATH] [--force] [--profile NAME]` | installs one skill bundle from a URL or local path (e.g. `library/cli`) |
| `update` | `usage: oswald skills update <name> [--ref SHA] [--force] [--profile NAME]` | refreshes to a new rev |
| `remove` | `usage: oswald skills remove <name> [--full] [--profile NAME]` | removes (source kept unless `--full`) |

## Notes

- One skill per directory; a directory holding several skills installs them
  one by one. `SKILL.md` frontmatter is `name/description/version`
  (+`required_env`); everything else is ignored so foreign skills load
  verbatim — convert them with the `skill-convert` skill first, never by
  hand-copying.
- Slugs match `[a-z0-9][a-z0-9_-]{0,63}`; dirs are `0700`, files `0600`.
- Nothing auto-installs: install only on explicit operator instruction, then
  verify with `oswald skills list` and one `skill_manage view` per skill.

## Effects

Skill changes take effect on the **next turn** (the catalog scans fresh on
every call — no cache to invalidate, no restart needed).

## Examples

```
oswald skills list
oswald skills install library/cli
oswald skills info cli
oswald skills update cli --ref abc1234
oswald skills remove cli
```

## Failures

- Bad source, name collisions (disambiguated by relative path, never
  guessed), or malformed `SKILL.md`: exit 1; install is atomic (publish or
  nothing).
