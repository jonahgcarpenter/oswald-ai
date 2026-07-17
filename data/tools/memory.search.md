# memory.search

## Description

Search stored memories for the current user when remembered user-specific context could improve the answer. You should retrieve memory proactively; the user does not need to ask what you remember.

Use this for the user's preferences, projects, identity details, environment, relationships, prior corrections, and user-specific instructions that may affect the response.

Do not use this as web search, public knowledge lookup, general conversation history, or a way to inspect Oswald's own identity/directives. Use web.search for current public facts and soul.read for Oswald's own identity.

## Parameters

| Name     | Type    | Required | Description                                                                                                                                                                |
| -------- | ------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| query    | string  | no       | Semantic search query. Omit only when filtering by scope/category is enough.                                                                                               |
| scope    | string  | no       | Optional scope filter: `short_term` or `long_term`.                                                                                                                        |
| category | string  | no       | Optional category filter: `identity`, `system_rules`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, or `notes`. |
| limit    | integer | no       | Maximum memories to return; defaults to 8.                                                                                                                                 |
