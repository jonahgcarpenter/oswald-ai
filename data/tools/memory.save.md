# memory.save

## Description

Explicitly save a grounded memory about the current user when the user asks you to remember something or when a durable correction must be recorded. Routine memory extraction happens automatically after turns; do not call this tool for ordinary conversation details.

## Parameters

| Name       | Type    | Required | Description                                                                                                                              |
| ---------- | ------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| statement  | string  | yes      | Concise third-person declarative memory, using `the user` instead of first person or second person.                                      |
| evidence   | string  | no       | Verbatim quote or concise evidence grounding the memory.                                                                                 |
| scope      | string  | no       | `short_term` for active/temporary context or `long_term` for durable facts; defaults to `short_term`.                                    |
| category   | string  | no       | Category: `identity`, `system_rules`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, `tasks`, or `notes`. |
| importance | integer | no       | Importance from 1 to 5; defaults to 3.                                                                                                   |
| confidence | number  | no       | Confidence from 0.0 to 1.0; defaults to 0.9 for explicit saves.                                                                          |
| ttl_days   | integer | no       | Expiration in days for `short_term` memories. Use 0 for default TTL or long-term memories.                                               |
| supersedes | string  | no       | Exact older statement this memory replaces, if the user corrected a prior memory.                                                        |
