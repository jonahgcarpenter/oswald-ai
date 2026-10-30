// Package config_manage owns the model-facing configuration tool: safe,
// mediated reads and writes of the profile's own config.yaml and .env. It
// bypasses workspace file groups (which deny direct config.yaml writes and
// seal .env) the same way the memory tool bypasses them for memories/: the
// tool is the mediated path. Every write validates through the exact startup
// pipeline before anything touches disk; secrets are redacted in all output.
package config_manage

import "github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"

// Name is the model-facing configuration tool name.
const Name = "config_manage"

// Definition returns the configuration tool's model-facing description and
// schema. Reads are redacted and always allowed; writes to the hot,
// recoverable subset apply inline, while secret-bearing or structural writes
// need the operator (the result carries the exact CLI command for them).
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name: Name,
		Description: `Read and safely modify your own profile configuration (config.yaml and .env). Direct file or shell edits to config.yaml and .env are denied by policy — this tool is the only mediated path.

EIGHT shapes, picked by action:
- show: effective configuration (or the subtree at path), secrets redacted. Start here before changing anything.
- get: print one schema path (e.g. 'model.default', 'platforms.discord.extra.allowed_users'), secrets redacted.
- set: validate and atomically write one leaf path (path plus value). Only log level and admission allow/ban/mention rules apply inline; anything else needs operator approval (see below).
- unset: clear one optional leaf path. Same approval rule as set.
- diff: current vs last-known-good, redacted.
- undo: restore last-known-good. Needs operator approval when the change touches anything outside the inline subset.
- env_show: list dotenv variable names only — values are never printed.
- env_set / env_unset: change one dotenv variable. Always needs operator approval (values are secrets by default).

APPROVAL: when a write needs the operator, the result is NOT applied — tell the user the exact CLI command from the result so they can run it themselves. Never print secret values in chat. Valid edits go live through auto-reload (log level and admission immediately, everything else flagged restart_required).`,
		Parameters: llm.ToolParameters{Type: "object", Required: []string{"action"}, Properties: map[string]llm.ToolParameterProperty{
			"action": {Type: "string", Enum: []string{"show", "get", "set", "unset", "diff", "undo", "env_show", "env_set", "env_unset"}, Description: "read or modify one configuration or environment entry."},
			"path":   {Type: "string", Description: "Schema path for show (optional subtree), get/set/unset (required leaf, e.g. 'runtime.log_level')."},
			"value":  {Type: "string", Description: "Set shape only: the new leaf value (bools, numbers, lists, and maps keep their types)."},
			"name":   {Type: "string", Description: "Env shapes only: the dotenv variable name."},
		}},
	}
}
