# user_memory_save

## Description

Stage up to five grounded memories about the authenticated current user from their current turn. Submit every independently useful direct fact or cautious inference in one call. The server verifies identity, exact evidence, authority, confidence, and activation policy, then publishes eligible candidates only after successful response delivery.

Call this proactively for durable identity, preference, project, relationship, environment, temporary-task, and correction facts. Use an empty memories array only when this tool is explicitly requested by a specialized extraction prompt and nothing is eligible.

Do not save trivial one-off conversation details, temporary wording, public facts, web search results, facts about unrelated people/groups, or Oswald's own identity/personality/directives. The operator controls Oswald's identity and standing behavior outside model tools.

For direct facts, use `user_statement`, quote the smallest exact evidence span beginning with a first-person marker, and write a concise third-person statement beginning with `The user` or `The user's`. For cautious implications, use `model_inference`, quote the complete current turn, and keep the statement governed by `may`, `might`, `likely`, `appears to`, or `seems to`. Never promote inference to direct evidence.

Use confidence `0.95` to `1.0` only for explicit unqualified facts, `0.70` to `0.89` for strong operational evidence, and `0.45` to `0.69` for plausible cautious inference. Omit candidates below `0.35`. Use `short_term` only for temporary task state with `ttl_days` from 1 to 30; otherwise use `long_term` and `ttl_days: 0`. Direct identity facts must have importance at least 3.

The tool returns an itemized JSON result. If an item is rejected, correct and retry only that item when the correction remains grounded in the same exact user evidence. Never resubmit accepted items or invent evidence to satisfy a rejection.

## Parameters

| Name     | Type  | Required | Description                                                                                           |
| -------- | ----- | -------- | ----------------------------------------------------------------------------------------------------- |
| memories | array | yes      | Zero to five independently grounded memory candidates. Each candidate is validated independently.    |

## Schema

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "memories": {
      "type": "array",
      "description": "Zero to five independently grounded memory candidates from the current user turn.",
      "minItems": 0,
      "maxItems": 5,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "statement": {"type": "string", "description": "Concise third-person declarative memory using the user instead of first or second person."},
          "evidence": {"type": "string", "description": "Exact verbatim quote from the current user turn. Inference evidence must be the complete turn."},
          "scope": {"type": "string", "enum": ["short_term", "long_term"]},
          "category": {"type": "string", "enum": ["identity", "communication_preferences", "durable_preferences", "projects", "relationships", "environment", "notes"]},
          "context": {"type": "string", "enum": ["direct_assertion", "temporary_task_state", "hypothetical", "quotation"]},
          "provenance": {"type": "string", "enum": ["user_statement", "model_inference", "third_party", "public_source", "tool_output"]},
          "sensitivity": {"type": "string", "enum": ["low", "identity_or_contact", "high_impact_interaction"]},
          "confidence": {"type": "number"},
          "importance": {"type": "integer"},
          "ttl_days": {"type": "integer"},
          "supersedes": {"type": "string", "description": "Exact older statement this candidate proposes to replace, or an empty string."},
          "claim_slot": {"type": "string", "description": "Stable category-compatible dotted semantic property."},
          "claim_value": {"type": "string", "description": "Concise normalized value grounded in the statement or evidence."}
        },
        "required": ["statement", "evidence", "scope", "category", "context", "provenance", "sensitivity", "confidence", "importance", "ttl_days", "supersedes", "claim_slot", "claim_value"]
      }
    }
  },
  "required": ["memories"]
}
```
