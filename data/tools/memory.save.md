# memory.save

## Description

Save a grounded memory about the current user when it is likely to remain useful beyond this turn. You should manage user memory proactively; the user does not need to explicitly say "remember this."

Use this for stable or recurring facts about the current user's identity, preferences, communication style, projects, relationships, environment, tasks, or standing instructions. Also use this when the user corrects something you previously believed or when a new fact should replace an older memory.

Do not save trivial one-off conversation details, temporary wording, public facts, web search results, facts about unrelated people/groups, or Oswald's own identity/personality/directives. Use soul tools for Oswald's own identity and standing behavior.

## Parameters

| Name       | Type    | Required | Description                                                                                                                                                |
| ---------- | ------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| statement  | string  | yes      | Concise third-person declarative memory, using `the user` instead of first person or second person.                                                        |
| evidence   | string  | no       | Verbatim quote or concise evidence grounding the memory.                                                                                                   |
| scope      | string  | no       | `short_term` for active/temporary context or `long_term` for durable facts; defaults to `short_term`.                                                      |
| category   | string  | no       | Category: `identity`, `system_rules`, `communication_preferences`, `durable_preferences`, `projects`, `relationships`, `environment`, `tasks`, or `notes`. |
| importance | integer | no       | Importance from 1 to 5; defaults to 3.                                                                                                                     |
| confidence | number  | no       | Confidence from 0.0 to 1.0; defaults to 0.9 for explicit saves.                                                                                            |
| ttl_days   | integer | no       | Expiration in days for `short_term` memories. Use 0 for default TTL or long-term memories.                                                                 |
| supersedes | string  | no       | Exact older statement this memory replaces, if the user corrected a prior memory.                                                                          |
