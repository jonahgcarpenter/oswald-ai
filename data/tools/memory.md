# memory

## Description

Save durable facts to persistent memory that survive across sessions. Memory is captured in the system prompt once per session; edits take effect in new sessions, not in the current session's frozen prompt. Keep entries compact and high-signal.

HOW: make ALL your changes in ONE call via an 'operations' array (each item: {action, content?, old_text?}). The batch applies atomically and the char limit is checked only on the FINAL result — so a single call can remove/replace stale entries to free room AND add new ones, even when an add alone would overflow. The response reports current/limit chars and confirms completion; one batch call finishes the update, so don't repeat it. Use the bare action/content/old_text fields only for a single lone change.

WHEN: only for facts that apply to EVERY session regardless of task: who the user is, stable environment facts, standing conventions with no task home. Anything learned while doing a task (procedures, pitfalls, and the user's preferences and corrections for that kind of work) belongs in the task's skill via skill_manage, where it loads only when relevant; memory is injected into every turn and must stay small.

IF FULL: an add is rejected with the current entries shown. Reissue as ONE batch that removes or shortens enough stale entries and adds the new one together.

TARGETS: 'user' = who the user is (name, role, preferences, style). 'memory' = your notes (environment, conventions, tool quirks, lessons).

SKIP: trivial/obvious info, easily re-discovered facts, raw data dumps, task progress, completed-work logs, temporary TODO state (use session_search for those). Reusable procedures belong in a skill, not memory.

## Parameters

| Name | Type | Required | Description |
| --- | --- | --- | --- |
| action | string | no | The action to perform (single-op shape). Omit when using 'operations'. |
| target | string | yes | Which memory store: 'memory' for personal notes, 'user' for user profile. |
| content | string | no | The entry content. Required for 'add' and 'replace'. For 'replace' it is the COMPLETE new entry text: the whole matched entry is overwritten, so include everything you want to keep. Alias: 'new_text' is also accepted (same full-entry meaning). |
| old_text | string | no | REQUIRED for 'replace' and 'remove' (single-op shape): a short unique substring IDENTIFYING the existing entry to modify -- it locates the entry, it is not spliced out. Omit only for 'add'. |
| new_text | string | no | Alias for 'content' (single-op shape): the COMPLETE new entry for 'replace', not a patch of old_text. If both are set, 'content' wins. |
| operations | array | no | Batch shape: a list of operations applied atomically in one call against the final char budget. Preferred when making multiple changes or consolidating to make room. Each item is {action, content?, old_text?}. |

## Schema

```json
{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["add", "replace", "remove"], "description": "The action to perform (single-op shape). Omit when using 'operations'."},
    "target": {"type": "string", "enum": ["memory", "user"], "description": "Which memory store: 'memory' for personal notes, 'user' for user profile."},
    "content": {"type": "string", "description": "The entry content. Required for 'add' and 'replace'. For 'replace' it is the COMPLETE new entry text: the whole matched entry is overwritten, so include everything you want to keep. Alias: 'new_text' is also accepted (same full-entry meaning)."},
    "old_text": {"type": "string", "description": "REQUIRED for 'replace' and 'remove' (single-op shape): a short unique substring IDENTIFYING the existing entry to modify -- it locates the entry, it is not spliced out. Omit only for 'add'."},
    "new_text": {"type": "string", "description": "Alias for 'content' (single-op shape): the COMPLETE new entry for 'replace', not a patch of old_text. If both are set, 'content' wins."},
    "operations": {
      "type": "array",
      "description": "Batch shape: a list of operations applied atomically in one call against the final char budget. Preferred when making multiple changes or consolidating to make room. Each item is {action, content?, old_text?}.",
      "items": {
        "type": "object",
        "properties": {
          "action": {"type": "string", "enum": ["add", "replace", "remove"]},
          "content": {"type": "string", "description": "Entry content for add/replace. For replace, the COMPLETE new entry (whole entry is overwritten). Alias: 'new_text'."},
          "new_text": {"type": "string", "description": "Alias for 'content' in a batch op."},
          "old_text": {"type": "string", "description": "Substring identifying the entry for replace/remove."}
        },
        "required": ["action"]
      }
    }
  },
  "required": ["target"]
}
```
