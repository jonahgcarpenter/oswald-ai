// Package delegate owns the model-facing delegate tool: spawning scoped
// subagent turns, and steering, stopping, and listing live subagents.
package delegate

import "github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"

// Name is the model-facing delegate tool name.
const Name = "delegate"

// MaxTasksPerCall bounds one batch. Larger batches fail closed instead of
// being truncated, so the parent always knows exactly what was dispatched.
const MaxTasksPerCall = 32

// MaxImagesPerTask bounds image attachments per task. Unreadable entries are
// skipped best-effort and never fail the dispatch.
const MaxImagesPerTask = 8

// Description is the model-facing delegate contract: fan work out to fresh
// subagent turns with isolated context. Every child starts with no knowledge
// of the parent conversation — put everything it needs in goal + context.
const Description = `Delegate work to subagent turns with fresh, isolated context. Each child gets its own conversation, tool budget, and working state; only its final summary returns to this turn. Use it for research, exploration, or multi-step procedures that would flood this context with intermediate output.

Spawn shapes (omit action, or set it to "spawn"):
- Single task: goal (what to achieve) + context (every fact, file path, error, and constraint it needs — the child knows nothing else).
- Batch: tasks[] with 1-10 entries (each its own goal + context). All run in parallel; one consolidated result returns when every task finishes, in input order with TASK i/N labels. No single task runs twice and none is silently dropped.

Optional per task: output_schema (a JSON object the final answer must validate as; the child retries once on validation failure, and unparseable answers come back raw marked schema_valid:false), images (up to 8 local paths or URLs the child must see; unreadable entries are skipped).

Control shapes (action set, spawn fields ignored):
- list: show this conversation's live subagents (id, goal, status).
- steer: subagent_id + message queues guidance into a running child at its next iteration boundary. A steer that lands after the child finished reports rejected.
- stop: subagent_id ends a child early at its next iteration boundary; its partial result returns as a normal completion.

Rules: the child inherits this turn's enabled toolsets minus clarify, memory, session_search, cronjob_manage, and delegate itself (no nested delegation, no user interaction, no shared-memory writes, no parent-history reads, no scheduling). Keep goals self-contained and verifiable; confirmation, approval, and user questions stay in this turn.`

// Definition returns the delegate tool's model-facing schema.
func Definition() llm.ToolDefinition {
	taskProperties := map[string]llm.ToolParameterProperty{
		"goal":          {Type: "string", Description: "What the task must achieve."},
		"context":       {Type: "string", Description: "Every fact the child needs: files, errors, paths, constraints. The child knows nothing else."},
		"output_schema": {Type: "object", Description: "Optional JSON object the final answer must validate as."},
		"images": {
			Type:        "array",
			Description: "Optional image paths or URLs the child must see (max 8; unreadable entries are skipped).",
			MaxItems:    intPtr(MaxImagesPerTask),
			Items:       &llm.ToolParameterProperty{Type: "string"},
		},
	}
	return llm.ToolDefinition{
		Name:        Name,
		Description: Description,
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"action":  {Type: "string", Enum: []string{"spawn", "list", "steer", "stop"}, Description: "Spawn shapes dispatch work (default spawn); list/steer/stop control this conversation's live subagents."},
			"goal":    {Type: "string", Description: "Single-task shape: what the child must achieve. Mutually exclusive with tasks."},
			"context": {Type: "string", Description: "Single-task shape: every fact the child needs. Mutually exclusive with tasks."},
			"tasks": {
				Type:        "array",
				Description: "Batch shape: independent tasks run in parallel with one consolidated result. Mutually exclusive with goal/context.",
				MinItems:    intPtr(1),
				MaxItems:    intPtr(MaxTasksPerCall),
				Items: &llm.ToolParameterProperty{
					Type:       "object",
					Required:   []string{"goal"},
					Properties: taskProperties,
				},
			},
			"output_schema": {Type: "object", Description: "Single-task shape: JSON object the final answer must validate as."},
			"images": {
				Type:        "array",
				Description: "Single-task images (max 8; unreadable entries are skipped).",
				MaxItems:    intPtr(MaxImagesPerTask),
				Items:       &llm.ToolParameterProperty{Type: "string"},
			},
			"role":        {Type: "string", Enum: []string{"leaf", "orchestrator"}, Description: "Leaf children cannot delegate further. Orchestrator keeps the delegate tool when nesting is enabled in config; flat configs run it as leaf."},
			"subagent_id": {Type: "string", Description: "Control shapes: the live subagent id from a dispatch or list result."},
			"message":     {Type: "string", Description: "Steer shape: guidance queued into the running child."},
		}},
	}
}

func intPtr(n int) *int { return &n }
