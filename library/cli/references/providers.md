# `oswald providers`

Manage the `providers.*` map: key transports plus ChatGPT-subscription and
Claude-SDK auth. Reads take `--profile`; writes fence to the calling scope.

## Commands

| Command | Syntax | Effect |
|---|---|---|
| `list` | `usage: oswald providers list [--profile NAME]` | names + API host, keys redacted |
| `show` | `usage: oswald providers show <name> [--profile NAME]` | one entry, key redacted |
| `add` | `usage: oswald providers add <name> --api URL [--key REF] [--auth key\|chatgpt-oauth\|claude-sdk] [--profile NAME]` | staged add (defaults `--auth key`) |
| `edit` | `usage: oswald providers edit <name> [--api URL] [--key REF] [--auth KIND] [--clear-key] [--clear-api] [--profile NAME]` | patch leaves in place |
| `unset` | `usage: oswald providers unset <name> key\|api\|auth [--profile NAME]` | clear one leaf (sugar over `--clear-*`) |
| `remove` | `usage: oswald providers remove <name> [--profile NAME]` | delete the whole entry; refused while `model.provider` references it |
| `auth` | `usage: oswald providers auth <name> [--no-browser] [--timeout DURATION] [--profile NAME]` | single-shot browser OAuth for `chatgpt-oauth` (default timeout 5m) |
| `auth start` | `oswald providers auth start <name> [--timeout DURATION] [--profile NAME]` | model-led handoff: prints sign-in URL (default 15m) |
| `auth finish` | `oswald providers auth finish <name> <code\|url> [--profile NAME]` | completes the handoff with the pasted response |
| `test` | `usage: oswald providers test <name> [--profile NAME]` | read-only login check |
| `status` | `usage: oswald providers status [--profile NAME]` | one-line auth state for every provider |
| `logout` | `usage: oswald providers logout <name> [--profile NAME]` | forgets a ChatGPT login (tokens + pending handoff) |

## Auth kinds

- `key` (default): needs `--api URL` (`http(s)://host[:port][/prefix]`, no
  userinfo/query/fragment) and optional `--key REF` (literal or `${VAR}`).
- `chatgpt-oauth`: transport pinned to the Codex backend — takes **no**
  `--api` and no `--key`. Tokens live in the profile store, never config.
- `claude-sdk`: shells to the installed `claude` CLI (`claude auth login`
  on-host, `claude setup-token` headless); `--api` optional. Logins stay
  owned by the `claude` CLI — `logout` refuses non-ChatGPT providers.

## Effects

Provider changes apply **live**. `remove` of the `model.provider` entry is
refused — switch the model first, then remove.

## Examples

```
oswald providers status
oswald providers add openai --api https://api.openai.com/v1 --key '${OPENAI_KEY}'
oswald providers add sub --auth chatgpt-oauth
oswald providers auth start sub
oswald providers auth finish sub <code|url>
oswald providers edit openai --key '${OPENAI_KEY_V2}'
oswald providers unset openai key
oswald providers remove openai
```

## Failures

- Subscription kinds reject `--key`; `chatgpt-oauth` rejects `--api`;
  `add` refuses duplicates; `unset api` on a key provider fails validation.
  Validation failures leave the file byte-identical with a `config_*` code.
