# memory.search

## Description

Run deeper hybrid lexical and semantic search over stored memories for the authenticated current user when bounded automatic recall is insufficient. This remains available in direct and group conversations; the user does not need to ask what you remember.

Results expose confidence, formation provenance, source authority, epistemic status, and sensitivity. Treat `uncertain_inference` as a hypothesis rather than an established fact.

Use this for the user's preferences, projects, identity details, environment, relationships, prior corrections, and user-specific instructions that may affect the response.

Do not use this as web search, public knowledge lookup, general conversation history, or a way to inspect Oswald's own identity/directives. Use web.search for current public facts and soul.read for Oswald's own identity.

## Parameters

| Name     | Type    | Required | Description                                                                                                                                                                |
| -------- | ------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| query    | string  | no       | Hybrid lexical and semantic search query. Omit only when filtering by scope/category is enough.                                                                            |
| scope    | string  | no       | Optional scope filter: `short_term` or `long_term`.                                                                                                                        |
| category | string  | no       | Optional category filter: `identity`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, or `notes`. |
| limit    | integer | no       | Maximum memories to return; defaults to 8.                                                                                                                                 |
