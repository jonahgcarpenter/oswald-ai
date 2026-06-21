# memory.list

## Description

List active memories stored about the current user. Use when the user asks what you remember or wants to inspect memory.

## Parameters

| Name     | Type    | Required | Description                                                                                                                                               |
| -------- | ------- | -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| scope    | string  | no       | Optional scope filter: `short_term` or `long_term`.                                                                                                       |
| category | string  | no       | Optional category filter: `identity`, `system_rules`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, `tasks`, or `notes`. |
| limit    | integer | no       | Maximum memories to return; defaults to 25.                                                                                                               |
