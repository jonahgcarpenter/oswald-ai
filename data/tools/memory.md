# memory

## Description

Edit the authenticated user's private USER.md or MEMORY.md. Both files are already included in your context; there is no read action. USER.md is limited to 1375 Unicode characters and MEMORY.md to 2200. Entries are separated by a standalone § line; never put § on its own line inside an entry. Changes are immediate. Choose `target` and either supply one `action` with its fields or an `operations` array. `old_text` must be a nonempty substring that identifies exactly one complete entry. Replace changes that whole entry; remove deletes it. Avoid duplicate or contradictory facts.

## Parameters

| Name | Type | Required | Description |
| --- | --- | --- | --- |
| target | string | yes | `user` (USER.md) or `memory` (MEMORY.md). |
| action | string | no | Single operation: `add`, `replace`, or `remove`. |
| content | string | no | New entry for add or replace. Cannot combine with new_text. |
| new_text | string | no | Alias for content on replace only. |
| old_text | string | no | Unique substring of an existing entry for replace/remove. |
| operations | array | no | 1-20 ordered operations instead of single-operation fields. |

## Schema

```json
{
  "type": "object",
  "properties": {
    "target": {"type": "string", "enum": ["user", "memory"]},
    "action": {"type": "string", "enum": ["add", "replace", "remove"]},
    "content": {"type": "string"},
    "new_text": {"type": "string"},
    "old_text": {"type": "string"},
    "operations": {
      "type": "array", "minItems": 1, "maxItems": 20,
      "items": {
        "type": "object",
        "properties": {
          "action": {"type": "string", "enum": ["add", "replace", "remove"]},
          "content": {"type": "string"},
          "new_text": {"type": "string"},
          "old_text": {"type": "string"}
        },
        "required": ["action"],
        "additionalProperties": false
      }
    }
  },
  "required": ["target"],
  "additionalProperties": false
}
```
