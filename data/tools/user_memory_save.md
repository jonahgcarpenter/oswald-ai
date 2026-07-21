# user_memory_save

## Description

Stage a grounded memory only when the authenticated current user explicitly asks Oswald to remember it in their current turn. The server verifies authenticated identity, explicit first-party intent, and exact evidence, then publishes the candidate after the completed turn is persisted.

Do not call this proactively. A separate post-turn formation process handles confidence-scored direct and inferred memories. Use this tool for explicit identity, preference, project, relationship, environment, temporary-task, and correction requests.

Do not save trivial one-off conversation details, temporary wording, public facts, web search results, facts about unrelated people/groups, or Oswald's own identity/personality/directives. The operator controls Oswald's identity and standing behavior outside model tools.

## Parameters

| Name       | Type    | Required | Description                                                                                                                                                |
| ---------- | ------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| statement  | string  | yes      | Concise third-person declarative memory, using `the user` instead of first person or second person.                                                        |
| evidence   | string  | no       | Exact verbatim quote from the current user's explicit remember request. When omitted, the server derives the text following the explicit remember phrase.   |
| scope      | string  | no       | `short_term` for active/temporary context or `long_term` for durable facts; defaults to `short_term`.                                                      |
| category   | string  | no       | Category: `identity`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, or `notes`. Eligible durable identity and preference facts become automatic context in new or reset sessions. |
| importance | integer | no       | Importance from 1 to 5; defaults to 3.                                                                                                                     |
| confidence | number  | no       | Confidence from 0.0 to 1.0; defaults to 0.9 for explicit saves.                                                                                            |
| ttl_days   | integer | no       | Expiration in days for `short_term` memories. Use 0 for default TTL or long-term memories.                                                                 |
| supersedes | string  | no       | Exact older statement this memory proposes to replace. Replacement occurs atomically only when ordinary authority and confidence comparison permits it.     |
| claim_slot | string  | yes      | Stable category-compatible dotted semantic property shared with equivalent automatic facts, such as `communication.reply_style`.                            |
| claim_value | string | yes      | Concise normalized value shared with equivalent automatic facts, such as `concise`; every meaningful value token must be grounded in statement or evidence. |
