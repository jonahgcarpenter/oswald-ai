# soul_memory

## Description

Manages your soul file — the Markdown document that defines who you are. This file is your system prompt, read fresh on every request. It describes your identity, origin, personality, and behavioral directives.

Use this tool to read your current soul, rewrite it entirely, or append new information to it. Changes take effect immediately on the next message — you are reshaping yourself in real time.

Actions:
- **read** — Returns the full contents of your soul file. Use this before writing to understand what is already there.
- **write** — Replaces the entire soul file with the provided content. Use carefully; this overwrites everything.
- **append** — Adds the provided content to the end of your soul file without disturbing what is already there. Prefer this for incremental updates.

## Parameters

| Name    | Type   | Required | Description                                                                 |
| ------- | ------ | -------- | --------------------------------------------------------------------------- |
| action  | string | yes      | One of: read, write, append                                                 |
| content | string | no       | The content to write or append (required for write and append actions)      |
