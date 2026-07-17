# memory.list

## Description

List active memories stored about the current user. Use this when the user asks what you remember about them, wants to inspect stored memory, or wants to choose memories to correct or delete.

Do not use this for normal memory retrieval during conversation; use memory.search instead. Do not use this for Oswald's own identity/directives, public facts, or web lookup.

## Parameters

| Name     | Type    | Required | Description                                                                                                                                                                |
| -------- | ------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| scope    | string  | no       | Optional scope filter: `short_term` or `long_term`.                                                                                                                        |
| category | string  | no       | Optional category filter: `identity`, `system_rules`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, or `notes`. |
| limit    | integer | no       | Maximum memories to return; defaults to 25.                                                                                                                                |
