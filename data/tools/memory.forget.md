# memory.forget

## Description

Delete active stored memory about the current user. Use this when the user asks you to forget something, remove stored user information, or correct a stored memory by deleting the old statement.

Do not use this for ordinary conversation cleanup, public facts, or Oswald's own identity/personality/directives. Use soul.patch for Oswald's own standing behavior.

## Parameters

| Name      | Type   | Required | Description                                                                                                  |
| --------- | ------ | -------- | ------------------------------------------------------------------------------------------------------------ |
| statement | string | yes      | Exact memory statement to remove; use `all` to wipe all user memories only when explicitly asked.            |
| scope     | string | no       | Optional scope filter: `short_term` or `long_term`. Omit to delete matching active memories in either scope. |
