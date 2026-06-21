# memory.recall

## Description

Retrieve persistent memory about the current user across all conversations. Use remembered user context to tune tone, assumptions, and examples.

## Parameters

| Name     | Type    | Required | Description                                                                                                                                                                    |
| -------- | ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| category | string  | no       | Category to recall: `identity`, `preferences`, `system_rules`, or `notes`; omit to retrieve the full memory profile, which may include a system-managed speaker identity line. |
| query    | string  | no       | Semantic search query for retrieving only relevant remembered facts instead of the full profile.                                                                                |
| limit    | integer | no       | Maximum semantic matches to return; defaults to 5 and is clamped to 10.                                                                                                        |
