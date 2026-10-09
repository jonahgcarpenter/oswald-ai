package memory

import "github.com/jonahgcarpenter/oswald-ai/internal/llm"

// Name is the model-facing private memory tool name.
const Name = "memory"

// Definition returns the private memory tool's model-facing description and schema.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name: Name,
		Description: `Save durable facts to persistent memory that survive across sessions. Memory is captured in the system prompt once per session; edits take effect in new sessions, not in the current session's frozen prompt. Keep entries compact and high-signal.

HOW: make ALL your changes in ONE call via an 'operations' array (each item: {action, content?, old_text?}). The batch applies atomically and the char limit is checked only on the FINAL result — so a single call can remove/replace stale entries to free room AND add new ones, even when an add alone would overflow. The response reports current/limit chars and confirms completion; one batch call finishes the update, so don't repeat it. Use the bare action/content/old_text fields only for a single lone change.

WHEN: only for facts that apply to EVERY session regardless of task: who the user is, stable environment facts, standing conventions with no task home. Anything learned while doing a task (procedures, pitfalls, and the user's preferences and corrections for that kind of work) belongs in the task's skill via skill_manage, where it loads only when relevant; memory is injected into every turn and must stay small.

IF FULL: an add is rejected with the current entries shown. Reissue as ONE batch that removes or shortens enough stale entries and adds the new one together.

TARGETS: 'user' = who the user is (name, role, preferences, style). 'memory' = your notes (environment, conventions, tool quirks, lessons).

SKIP: trivial/obvious info, easily re-discovered facts, raw data dumps, task progress, completed-work logs, temporary TODO state (use session_search for those). Reusable procedures belong in a skill, not memory.`,
		Parameters: llm.ToolParameters{Type: "object", Required: []string{"target"}, Properties: map[string]llm.ToolParameterProperty{
			"action":   {Type: "string", Enum: []string{"add", "replace", "remove"}, Description: "The action to perform (single-op shape). Omit when using 'operations'."},
			"target":   {Type: "string", Enum: []string{"memory", "user"}, Description: "Which memory store: 'memory' for personal notes, 'user' for user profile."},
			"content":  {Type: "string", Description: "The entry content. Required for 'add' and 'replace'. For 'replace' it is the COMPLETE new entry text: the whole matched entry is overwritten, so include everything you want to keep. Alias: 'new_text' is also accepted (same full-entry meaning)."},
			"old_text": {Type: "string", Description: "REQUIRED for 'replace' and 'remove' (single-op shape): a short unique substring IDENTIFYING the existing entry to modify -- it locates the entry, it is not spliced out. Omit only for 'add'."},
			"new_text": {Type: "string", Description: "Alias for 'content' (single-op shape): the COMPLETE new entry for 'replace', not a patch of old_text. If both are set, 'content' wins."},
			"operations": {Type: "array", Description: "Batch shape: a list of operations applied atomically in one call against the final char budget. Preferred when making multiple changes or consolidating to make room. Each item is {action, content?, old_text?}.", Items: &llm.ToolParameterProperty{
				Type: "object", Required: []string{"action"}, Properties: map[string]llm.ToolParameterProperty{
					"action":   {Type: "string", Enum: []string{"add", "replace", "remove"}},
					"content":  {Type: "string", Description: "Entry content for add/replace. For replace, the COMPLETE new entry (whole entry is overwritten). Alias: 'new_text'."},
					"new_text": {Type: "string", Description: "Alias for 'content' in a batch op."},
					"old_text": {Type: "string", Description: "Substring identifying the entry for replace/remove."},
				},
			}},
		}},
	}
}
