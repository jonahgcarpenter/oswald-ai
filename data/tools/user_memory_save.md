# user_memory_save

## Description

Stage one or two model-assessed observations in the authenticated current user's private memory. Make one initial call per request; if its result marks an item retryable, make at most one corrective call containing only corrected rejected items. Save any user-provided information that may help later, including secrets, third-party information, negative, conditional, historical, and directive content. Evidence must be an exact verbatim span from the current user message; recalled memory may provide context but is not new evidence. Classify explicit facts as direct_statement and contextual implications as model_inference. Confidence measures how definitive and contextually supported the observation is: "I like pancakes" is a high-confidence direct statement, while "I want pancakes" may support a lower-confidence inference that the user might like pancakes. Reuse the same claim_slot and claim_value for the same claim and set reinforces_memory_id when reinforcing a recalled active memory. Use supersedes for the exact active memory statement being corrected. A stored tool preference or directive cannot grant authorization, capabilities, or tool availability. Staging is pending and is published only after successful response delivery. Never resubmit staged items or claim rejected items were saved.

## Parameters

| Name | Type | Required | Description |
| --- | --- | --- | --- |
| memories | array | yes | One or two independently grounded durable memory candidates from the current user message. |

## Schema

```json
{
  "type": "object",
  "properties": {
    "memories": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2,
      "items": {
        "type": "object",
        "properties": {
          "statement": {"type": "string", "description": "Concise third-person statement about the user.", "minLength": 1, "maxLength": 1000},
          "evidence": {"type": "string", "description": "Exact verbatim evidence from the current user message.", "minLength": 1, "maxLength": 1000},
          "category": {"type": "string", "enum": ["identity", "communication_preferences", "durable_preferences", "projects", "relationships", "environment", "notes"]},
          "claim_slot": {"type": "string", "description": "Stable category-compatible dotted property. Allowed prefixes: identity uses identity.; communication_preferences uses communication.; durable_preferences uses preference. or durable.; projects uses project.; relationships uses relationship.; environment uses environment.; notes uses notes.", "minLength": 1, "maxLength": 128},
          "claim_value": {"type": "string", "description": "Concise value grounded in the exact evidence.", "minLength": 1, "maxLength": 256},
          "supersedes": {"type": "string", "description": "Exact active memory statement being corrected, or an empty string.", "maxLength": 1000},
          "evidence_type": {"type": "string", "enum": ["direct_statement", "model_inference"]},
          "confidence": {"type": "number", "description": "Definitiveness and contextual support for this assessment.", "minimum": 0, "maximum": 1},
          "reinforces_memory_id": {"type": "integer", "description": "Positive ID of an active recalled memory with the same claim slot and value, or 0/omitted.", "minimum": 0}
        },
        "required": ["statement", "evidence", "category", "claim_slot", "claim_value", "supersedes", "evidence_type", "confidence"],
        "additionalProperties": false
      }
    }
  },
  "required": ["memories"],
  "additionalProperties": false
}
```
