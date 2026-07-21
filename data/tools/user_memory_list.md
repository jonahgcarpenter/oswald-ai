# user_memory_list

## Description

List active memories and stable canonical memory IDs stored for the authenticated current user. This remains available in direct and group conversations. Use it when the user asks what you remember about them or needs an exact ID for correction or deletion.

Entries include confidence, formation provenance, source authority, epistemic status, and sensitivity. Treat `uncertain_inference` as a hypothesis rather than an established fact.

Do not use this for normal memory retrieval during conversation; use user_memory_search instead. Do not use this for Oswald's own identity/directives, public facts, or web lookup.

## Parameters

| Name     | Type    | Required | Description                                                                                                                                                                |
| -------- | ------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| scope    | string  | no       | Optional scope filter: `short_term` or `long_term`.                                                                                                                        |
| category | string  | no       | Optional category filter: `identity`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, or `notes`. |
| limit    | integer | no       | Maximum memories to return; defaults to 25.                                                                                                                                |
