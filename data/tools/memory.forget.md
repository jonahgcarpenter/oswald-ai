# memory.forget

## Description

Delete active short-term or long-term memory about the current user. Only use when the user asks you to forget something or correct stored memory.

## Parameters

| Name      | Type   | Required | Description                                                                                                           |
| --------- | ------ | -------- | --------------------------------------------------------------------------------------------------------------------- |
| statement | string | yes      | Exact memory statement to remove; use `all` to wipe all user memories only when explicitly asked.                     |
| scope     | string | no       | Optional scope filter: `short_term` or `long_term`. Omit to delete matching active memories in either scope.          |
