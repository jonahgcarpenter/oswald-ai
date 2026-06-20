# memory.recall

## Description

Retrieve persistent memory about the current user across all conversations. Use remembered user context to tune tone, assumptions, and examples.

## Parameters

| Name     | Type   | Required | Description                                                                                                                                                                    |
| -------- | ------ | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| category | string | no       | Category to recall: `identity`, `preferences`, `system_rules`, or `notes`; omit to retrieve the full memory profile, which may include a system-managed speaker identity line. |
