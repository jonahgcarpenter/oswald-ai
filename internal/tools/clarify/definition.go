// Package clarify owns the model-facing clarify tool: a blocking,
// single-flight question form answered by the user mid-turn.
package clarify

import "github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"

// Name is the model-facing clarify tool name.
const Name = "clarify"

// Description is the model-facing clarify contract. The text renderer lists
// the first choice of each single-select question with a (Recommended)
// suffix and appends an Other row, so the UI claims below hold on
// transports without pickable controls.
const Description = `Ask the user one or more questions when you need a decision or feedback. Pass every question in ` + "`questions`" + ` (1-5 entries) — a single question is a one-entry array, and several INDEPENDENT questions belong in ONE call (one form beats a chain of clarify calls; if one answer would change another question, ask separately). Per question: single-select (up to 4 choices — put your recommended option FIRST, the UI marks it '(Recommended)' and auto-appends an 'Other' free-text row), multi-select (multi_select=true), or open-ended (omit choices). Keep choices short plain text with no emojis or markdown formatting. Options go ONLY in ` + "`choices`" + `, never enumerated inside the question text (choices render as pickable rows; options written into the question are dead prose the user can't click). Result: {responses: [...], outcome} in question order. Each response has status answered, skipped or unanswered (user_response is null unless answered); outcome is submitted, cancelled, timed_out or undelivered, with a notice saying why when the wait ended without a submit. Prefer deciding low-stakes questions yourself; don't use this for dangerous-command confirmation (the terminal tool handles that).`

// Definition returns the model-facing clarify tool schema.
func Definition() llm.ToolDefinition {
	one := 1
	four := 4
	five := 5
	return llm.ToolDefinition{
		Name:        Name,
		Description: Description,
		Parameters: llm.ToolParameters{
			Type:     "object",
			Required: []string{"questions"},
			Properties: map[string]llm.ToolParameterProperty{
				"questions": {
					Type:        "array",
					Description: "The question(s). Each: question text (options excluded), optional choices (recommended first; omit for free-text), optional multi_select. Responses come back in question order with the question text echoed.",
					MinItems:    &one,
					MaxItems:    &five,
					Items: &llm.ToolParameterProperty{
						Type:     "object",
						Required: []string{"question"},
						Properties: map[string]llm.ToolParameterProperty{
							"question": {Type: "string"},
							"choices": {
								Type:        "array",
								Items:       &llm.ToolParameterProperty{Type: "string"},
								MaxItems:    &four,
								Description: "Available choices (recommended first; omit for free-text)",
							},
							"multi_select": {Type: "boolean"},
						},
					},
				},
			},
		},
	}
}
