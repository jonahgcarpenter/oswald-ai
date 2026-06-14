# mcp.tools

## Description

List read-only tools available from one MCP server. Calling this tool makes the returned tools available for direct tool calls during the rest of the current request. Use the query parameter to avoid exposing an entire large server catalog when only one capability is needed.

## Parameters

| Name   | Type    | Required | Description |
| ------ | ------- | -------- | ----------- |
| server | string  | yes      | MCP server name, such as `github`. |
| query  | string  | no       | Optional capability or name filter, such as `file contents`, `issues`, or `pull requests`. |
| limit  | integer | no       | Maximum number of tools to return and expose. Defaults to 8 and is capped at 20. |
