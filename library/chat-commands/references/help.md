# `/help`

`Use: /help [command]` — list commands or show usage for one.

## Effects

1. Bare `/help` lists every command with its one-line summary.
2. `/help <command>` renders that command's summary plus its `Use:` line.
3. Unknown names answer `Unknown command: /<name>` with a `rejected` /
   `unknown_command` outcome — nothing runs.

## Notes

- The list is generated from the registered definitions, so it always
  matches the running server — quote it rather than memorizing command sets
  from elsewhere.
- Aliases resolve too (`/help` strips a leading `/` from its argument).

## Example

User: `/help model`
Assistant: `Show or set the model for this conversation` + `Use: /model [provider] | /model <provider>/<model> [--global]`
