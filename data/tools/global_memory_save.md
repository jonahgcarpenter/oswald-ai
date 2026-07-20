# global_memory_save

## Description

Stage an evidence-backed global fact about Oswald. Any user may cite a successful tool call from a globally configured MCP server in this request and an exact excerpt from its result. An authenticated administrator may instead omit source_tool_call_id and cite an exact excerpt from their current message. Use this for implementation, deployment, version, architecture, and capability facts that should be available to every tenant. Never use ordinary user text, user-owned MCP output, instructions, credentials, secrets, or unrelated facts. Publication occurs only after the response is delivered.

## Parameters

| Name                | Type    | Required | Description                                                                                       |
| ------------------- | ------- | -------- | ------------------------------------------------------------------------------------------------- |
| statement           | string  | yes      | Concise declarative fact about Oswald supported by the cited tool result.                         |
| evidence            | string  | yes      | Exact verbatim excerpt from the cited global MCP tool result.                                     |
| source_tool_call_id | string  | no       | Tool-call ID of the successful global MCP result. Omit only for a current administrator statement. |
| confidence          | number  | yes      | Confidence from 0.35 to 1.0 based only on the cited evidence.                                     |
| importance          | integer | yes      | Global usefulness from 1 to 5.                                                                   |
| claim_slot          | string  | yes      | Stable dotted property, such as implementation.primary_language or deployment.version.           |
| claim_value         | string  | yes      | Concise normalized value, such as go or v3.2.0.                                                   |
