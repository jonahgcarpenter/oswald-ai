# memory.remember

## Description

Store or update persistent third-person memory about the current user using grounded evidence.

## Parameters

| Name      | Type   | Required | Description                                                                                                                                                                                                 |
| --------- | ------ | -------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| statement | string | yes      | The memory as a concise third-person declarative sentence; avoid first-person or second-person phrasing and refer to the individual neutrally or as `the user`.                                            |
| evidence  | string | yes      | A verbatim quote or a summary of sustained engagement proving the statement; never make baseless assumptions, and append `Date: [...]` only when a specific date or range matters.                         |
| category  | string | no       | Category for the fact: `identity`, `preferences`, `system_rules`, or `notes`; defaults to `notes`.                                                                                                        |
