# comfyui.image_to_image

## Description

Transform the first image attached to the current request according to a detailed visual prompt. The returned image is attached to the response. Use negative_prompt only to describe visual elements that should be excluded.

## Parameters

| Name | Type | Required | Description |
| --- | --- | --- | --- |
| prompt | string | yes | Detailed positive description of the requested transformation |
| negative_prompt | string | no | Visual elements and qualities to exclude |

## Schema

```json
{
  "type": "object",
  "properties": {
    "prompt": {"type": "string", "description": "Detailed positive description of the requested transformation", "minLength": 1, "maxLength": 2000},
    "negative_prompt": {"type": "string", "description": "Visual elements and qualities to exclude", "maxLength": 2000}
  },
  "required": ["prompt"],
  "additionalProperties": false
}
```
