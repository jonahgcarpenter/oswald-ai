# soul.patch

## Description

Patch the soul file that defines the live identity, personality, and standing directives that define you, Oswald, by adding, replacing, or removing exactly one line. Use this only when the user explicitly asks to change Oswald's identity, behavior, persona, or standing instructions. Line matching is by exact full-line text only; if the target or anchor is missing or matches multiple lines, the tool must fail instead of guessing.

## Parameters

| Name      | Type   | Required | Description                                                                           |
| --------- | ------ | -------- | ------------------------------------------------------------------------------------- |
| operation | string | yes      | The patch operation to perform: add, replace, or remove.                              |
| target    | string | no       | Exact current line text to replace or remove. Required for replace/remove.            |
| content   | string | no       | New single line of Oswald soul text to add or replace with. Required for add/replace. |
| position  | string | no       | For add only: where to insert the line. One of before, after, or end.                 |
| anchor    | string | no       | For add with before/after: exact existing line text to insert relative to.            |
