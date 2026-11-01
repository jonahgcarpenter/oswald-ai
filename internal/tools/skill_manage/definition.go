// Package skill_manage owns the model-facing skill tool: discovery,
// on-demand loading, and maintenance of profile skills.
package skill_manage

import "github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"

// Name is the model-facing skill tool name.
const Name = "skill_manage"

// Definition returns the skill tool's model-facing description and schema.
// Discovery is a compact index (list); bodies load on demand (view) so a
// large library never bloats context.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name: Name,
		Description: `Load and maintain reusable procedural skills. Skills hold per-task how-to knowledge (procedures, pitfalls, conventions, proven workflows) that does not belong in memory: memory is injected into every turn and must stay small, while skills load on demand.

FOUR shapes, picked by action:
- list: the compact name + description (+ category) index. Start here when you need a procedure you don't have.
- view: load one skill's full body by name (as listed). Load a skill whenever it matches or is even partially relevant to your task, and follow its instructions — err on the side of loading. Pass file_path to read one of its supporting files instead.
- create: add a new skill (name, description, body) after a difficult or iterative task worth reusing, or when the user teaches you a lasting procedure. Pass an optional single-word category (e.g. "code", "writing") to organize it; omit it for the general category.
- update: fix a skill whose steps were missing, wrong, or incomplete (old_text must match exactly once; it locates the text, content is the replacement). Name and description lines are editable the same way (description max 60 chars, one line). A successful update auto-bumps the patch version, so never hand-edit version:.

RULES: reusable procedures live in skills, never in memory. If a loaded skill was missing steps, had wrong commands, or needed pitfalls you discovered, update it before finishing.`,
		Parameters: llm.ToolParameters{Type: "object", Required: []string{"action"}, Properties: map[string]llm.ToolParameterProperty{
			"action":      {Type: "string", Enum: []string{"list", "view", "create", "update"}, Description: "list the index, view one skill body, create a new skill, or update an existing skill."},
			"name":        {Type: "string", Description: "Skill name as listed (view/update). Required for create (new skill name) and update."},
			"file_path":   {Type: "string", Description: "View shape only: one supporting file inside the skill (e.g. 'references/api.md'). Omit to read the main body."},
			"description": {Type: "string", Description: "Create shape only: one-line description of when to load the skill. Max 60 characters."},
			"content":     {Type: "string", Description: "Create shape: the full skill body. Update shape: the replacement text for old_text."},
			"category":    {Type: "string", Description: "Create shape only: optional single-level category grouping the skill (e.g. 'code'). Omit for the general category."},
			"old_text":    {Type: "string", Description: "Update shape only: exact unique substring of the skill file to replace — it locates the text, it is not spliced out."},
		}},
	}
}
