# memory.search

## Description

Search short-term and long-term memory for the current user. Use this when a response needs remembered context that is not already in the prompt.

## Parameters

| Name     | Type    | Required | Description                                                                                                                                               |
| -------- | ------- | -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| query    | string  | no       | Semantic search query. Omit only when filtering by scope/category is enough.                                                                               |
| scope    | string  | no       | Optional scope filter: `short_term` or `long_term`.                                                                                                       |
| category | string  | no       | Optional category filter: `identity`, `system_rules`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, `tasks`, or `notes`. |
| limit    | integer | no       | Maximum memories to return; defaults to 8.                                                                                                                |
