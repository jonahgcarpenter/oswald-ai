# `/model`

`Use: /model [provider] | /model <provider>/<model> [--global]` — show or
set the model for this conversation.

## Effects

1. Bare `/model` shows `Current: <provider>/<model>` (with `(context N)`
   when the window was reported) plus the configured providers.
2. `/model <provider>` lists that provider's models live (capped at 40 with
   an ellipsis note).
3. `/model <provider>/<model>` pins the conversation — live from the next
   turn, survives `/new`, changed only by setting another model. The pin
   resolves per turn: pin, then channel override, then profile default; a
   degraded link logs a warning and falls back instead of failing.
4. With `--global`, writes the acting profile's `model.provider` +
   `model.default` through the mediated config path (staged validation,
   last-known-good, one atomic commit), applies live via the reload applier
   on its next watcher tick, and pins the acting conversation. Runs under
   the per-profile writer fence.

## Lifespan

Pins die with the conversation (except across `/new`, which they survive).
`--global` persists in `config.yaml`, independent of any session.

## Interactions

- Suggest `--global` only when the user wants the change to stick for every
  future conversation; otherwise the pin is the right scope.
- A mid-turn provider switch via CLI still completes the running turn on
  the old transport — the new routing takes effect next turn.

## Example

User: `/model`
Assistant: `Current: custom:fake/fake/model (context 32768). Configured: fake, other.`
User: `/model other/other-model --global`
Assistant: writes config, confirms `applied live`, pins this conversation too.

## Deny cases

- Unknown provider or model: rejected with the provider list, nothing
  pinned. `--global` under a scope that may not mutate is denied by the
  profile writer fence.
