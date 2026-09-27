# web_search

## Description

Search the web for information. Returns up to 5 results by default with titles, URLs, and descriptions. The query is passed through to the configured backend, so operators such as site:domain, filetype:pdf, intitle:word, -term, and "exact phrase" may work when the backend supports them.

## Parameters

| Name  | Type    | Required | Description |
| ----- | ------- | -------- | ----------- |
| query | string  | yes      | The search query to look up on the web. You may include backend-supported operators such as site:example.com, filetype:pdf, intitle:word, -term, or "exact phrase". |
| limit | integer | no       | Maximum number of results to return. Defaults to 5. |

## Schema

```json
{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "The search query to look up on the web. You may include backend-supported operators such as site:example.com, filetype:pdf, intitle:word, -term, or \"exact phrase\"."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of results to return. Defaults to 5.",
      "default": 5,
      "minimum": 1,
      "maximum": 100
    }
  },
  "required": ["query"],
  "additionalProperties": false
}
```
