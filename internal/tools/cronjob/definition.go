package cronjob

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

// Name is the model-facing tool name. The value is the persisted contract;
// only the owning package moved.
const Name = "cronjob_manage"

// CronjobManageDefinition returns the scheduler contract. Model/provider
// overrides are intentionally absent: job identity is user-owned and the
// agent must not set it. Schedules are stored as UTC wall-clock instants;
// the model resolves event-anchored times arithmetically in UTC and verifies
// wall-clock conversions with the terminal before scheduling.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Manage scheduled jobs: create, list, update, pause, resume, remove, or run now. Schedules accept durations (30m), every-phrases (every 2h, every monday 9am), cron (0 9 * * *), RFC3339 one-shots, or in-phrases (in 30m); stored times are UTC. When the request ties to a real-world instant (kickoff, tipoff, market open), resolve that instant in UTC and offset arithmetically — no timezone involved. For a named wall-clock time, use the stated zone, or the recalled zone when the user named none before; when the zone is neither stated nor recalled, ask via the clarify tool first — ask, don't guess. Before create/update with a wall-clock-derived schedule, verify with the terminal (TZ=<zone> date -d '<local time>' '+%Y-%m-%dT%H:%M:%SZ') and use its output verbatim; never hand-add offsets. After create/update, convert next_run_at back (TZ=<zone> date -d '<utc>') and confirm the local time to the user. Durations need no confirmation. Prompts must be self-contained: the final response is delivered and cannot ask questions. Use script (a bare name under the profile scripts/ directory) for stdout-injected work (no_agent=true runs the script as the job, empty output sends nothing). Jobs run agent turns on a schedule and deliver results to the concrete deliver target. Operate by job_id or unique name; ambiguous names error instead of guessing.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"action":           {Type: "string", Description: "create|list|update|pause|resume|remove|run"},
			"job_id":           {Type: "string", Description: "Job ID or unique name for update/pause/resume/remove/run"},
			"name":             {Type: "string", Description: "Human job name for create/update"},
			"schedule":         {Type: "string", Description: "30m, every 2h, every monday 9am, 0 9 * * *, RFC3339 one-shot, or in 30m. Wall-clock forms are stored as UTC — event-anchored times need no zone (offset in UTC); a stated or recalled zone needs no question, only terminal verification; ask via clarify only when the zone is neither stated nor recalled; in-phrases and durations need no zone"},
			"prompt":           {Type: "string", Description: "Self-contained prompt; the final response is delivered and cannot ask questions. Omit when no_agent is true"},
			"skills":           {Type: "array", Description: "Skill names available to the run", Items: &llm.ToolParameterProperty{Type: "string"}},
			"script":           {Type: "string", Description: "Script name under the profile scripts/ directory; stdout is injected (no_agent=true runs it as the job)"},
			"no_agent":         {Type: "boolean", Description: "Script is the job; stdout verbatim, empty sends nothing", Default: false},
			"deliver_platform": {Type: "string", Description: "Delivery platform: discord (native @-mentions) or imessage (name prefix only, no notify-tap). Omit with deliver_chat_id to default to the creating chat"},
			"deliver_chat_id":  {Type: "string", Description: "Delivery channel/chat. Omit with deliver_platform to default to the creating chat"},
			"deliver_user_id":  {Type: "string", Description: "Transport-native user ID for groupchat mention (Discord numeric snowflake, iMessage handle); ignored in DMs. Never a display name. Discord sends one '<@user> - <job name>' message with the body on the next line; iMessage sends a tappable '<profile> - <job name>' mention bubble with the result threaded underneath."},
			"enabled_toolsets": {Type: "array", Description: "Builtin toolset allowlist from core,terminal,code_execution,cronjob ([] = zero builtins)", Items: &llm.ToolParameterProperty{Type: "string"}},
			"mcp_servers":      {Type: "array", Description: "MCP server allowlist by exact config name (omit = all profile servers; [] = no MCP; bridge tools appear only when servers are reachable)", Items: &llm.ToolParameterProperty{Type: "string"}},
			"repeat_times":     {Type: "integer", Description: "Run N times then auto-complete (omit = forever for recurring, once for one-shots; 0 = forever)", Minimum: float64Ptr(0)},
			"paused":           {Type: "boolean", Description: "Create paused with a reason via paused_reason"},
			"paused_reason":    {Type: "string", Description: "Reason when creating paused or pausing"},
		}, Required: []string{"action"}, AdditionalProperties: boolPtr(false)},
	}
}

func float64Ptr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }
