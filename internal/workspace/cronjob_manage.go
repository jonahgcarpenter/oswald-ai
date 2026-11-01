package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	outputpkg "github.com/jonahgcarpenter/oswald-ai/internal/tools/output"
)

// CronjobManageDefinition returns the scheduler contract. Model/provider
// overrides are intentionally absent: job identity is user-owned and the
// agent must not set it. Schedules are UTC wall-clock: the model converts
// local intent before scheduling.
func CronjobManageDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        CronjobManageName,
		Description: "Manage scheduled jobs: create, list, update, pause, resume, remove, or run now. Schedules accept durations (30m), every-phrases (every 2h, every monday 9am), cron (0 9 * * *), RFC3339 one-shots, or in-phrases (in 30m); stored times are UTC. If the request names a wall-clock time without an explicit zone, confirm the user's location/timezone first — ask, don't guess. Durations need no confirmation. Prompts must be self-contained: the final response is delivered and cannot ask questions. Use script (a bare name under the profile scripts/ directory) for stdout-injected work (no_agent=true runs the script as the job, empty output sends nothing). Jobs run agent turns on a schedule and deliver results to the concrete deliver target. Operate by job_id or unique name; ambiguous names error instead of guessing.",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"action":           {Type: "string", Description: "create|list|update|pause|resume|remove|run"},
			"job_id":           {Type: "string", Description: "Job ID or unique name for update/pause/resume/remove/run"},
			"name":             {Type: "string", Description: "Human job name for create/update"},
			"schedule":         {Type: "string", Description: "30m, every 2h, every monday 9am, 0 9 * * *, RFC3339 one-shot, or in 30m. Wall-clock forms are stored as UTC — confirm the zone with the user when unstated; in-phrases and durations need no zone"},
			"prompt":           {Type: "string", Description: "Self-contained prompt; the final response is delivered and cannot ask questions. Omit when no_agent is true"},
			"skills":           {Type: "array", Description: "Skill names available to the run", Items: &llm.ToolParameterProperty{Type: "string"}},
			"script":           {Type: "string", Description: "Script name under the profile scripts/ directory; stdout is injected (no_agent=true runs it as the job)"},
			"no_agent":         {Type: "boolean", Description: "Script is the job; stdout verbatim, empty sends nothing", Default: false},
			"deliver_platform": {Type: "string", Description: "Delivery platform: discord (native @-mentions) or imessage (name prefix only, no notify-tap). Omit with deliver_chat_id to default to the creating chat"},
			"deliver_chat_id":  {Type: "string", Description: "Delivery channel/chat. Omit with deliver_platform to default to the creating chat"},
			"deliver_user_id":  {Type: "string", Description: "Transport-native user ID for groupchat mention (Discord numeric snowflake, iMessage handle); ignored in DMs. Never a display name"},
			"enabled_toolsets": {Type: "array", Description: "Builtin toolset allowlist from core,terminal,code_execution,cronjob ([] = zero builtins)", Items: &llm.ToolParameterProperty{Type: "string"}},
			"mcp_servers":      {Type: "array", Description: "MCP server allowlist by exact config name (omit = all profile servers; [] = no MCP; bridge tools appear only when servers are reachable)", Items: &llm.ToolParameterProperty{Type: "string"}},
			"repeat_times":     {Type: "integer", Description: "Run N times then auto-complete (omit = forever for recurring, once for one-shots; 0 = forever)", Minimum: float64Ptr(0)},
			"paused":           {Type: "boolean", Description: "Create paused with a reason via paused_reason"},
			"paused_reason":    {Type: "string", Description: "Reason when creating paused or pausing"},
		}, Required: []string{"action"}, AdditionalProperties: boolPtr(false)},
	}
}

// NewCronjobManageHandler returns the scheduler handler.
func NewCronjobManageHandler(ws *Workspace, store *cron.Store) func(context.Context, map[string]interface{}) (governance.Result, error) {
	runner := &cron.Runner{}
	if ws != nil {
		runner.Root = ws.Root()
		runner.Resolve = func(raw string) (string, error) {
			abs, _, err := ws.Resolve(raw)
			return abs, err
		}
		runner.ResolveDir = func(raw string) (string, error) { return ws.ResolveDir(raw) }
		runner.ScriptsDir = filepath.Join(ws.Root(), "scripts")
		runner.Scratch = ws.ScratchDir()
	}
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		if ws == nil || ws.Root() == "" || store == nil {
			return governance.Result{}, errors.New("workspace is unavailable")
		}
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("workspace: authenticated principal required")
		}
		if err := rejectUnknown(args, "action", "job_id", "name", "schedule", "prompt", "skills", "script", "no_agent", "deliver_platform", "deliver_chat_id", "deliver_user_id", "enabled_toolsets", "mcp_servers", "repeat_times", "paused", "paused_reason", "model", "provider", "base_url"); err != nil {
			return governance.Result{}, err
		}
		if _, has := args["model"]; has {
			return governance.Result{}, errors.New("model override is user-owned; omit unless the user explicitly asked")
		}
		if _, has := args["provider"]; has {
			return governance.Result{}, errors.New("provider override is user-owned; omit unless the user explicitly asked")
		}
		if _, has := args["base_url"]; has {
			return governance.Result{}, errors.New("base_url override is user-owned; omit unless the user explicitly asked")
		}
		action, _ := stringArg(args, "action")
		switch strings.TrimSpace(action) {
		case "list":
			jobs, err := store.List()
			if err != nil {
				return governance.Result{}, err
			}
			if len(jobs) == 0 {
				return governance.Result{Content: "(no jobs)", Outcome: governance.OutcomeUnproductive}, nil
			}
			var sb strings.Builder
			for _, job := range jobs {
				next := "-"
				if job.NextRunAt != nil && strings.TrimSpace(*job.NextRunAt) != "" {
					next = strings.TrimSpace(*job.NextRunAt)
				}
				fmt.Fprintf(&sb, "%s %s %s state=%s next=%s mcp=%s status=%s\n", job.ID, outputpkg.Clean(job.Name), outputpkg.Clean(job.Schedule.Display), job.State, next, mcpDisplay(job.MCPServers), job.LastStatus)
			}
			return governance.Result{Content: strings.TrimSuffix(sb.String(), "\n"), Outcome: governance.OutcomeProductive}, nil
		case "create":
			if _, ok := args["repeat_times"]; ok {
				if _, err := intArg(args, "repeat_times", 0); err != nil {
					return governance.Result{}, errors.New("repeat_times must be an integer >= 0")
				}
			}
			input, err := decodeCronInput(ctx, args)
			if err != nil {
				return governance.Result{}, err
			}
			job, err := store.Create(time.Now().UTC(), runner, input)
			if err != nil {
				return governance.Result{}, err
			}
			next := "-"
			if job.NextRunAt != nil {
				next = *job.NextRunAt
			}
			return governance.Result{Content: fmt.Sprintf("job_id: %s\nnext_run_at: %s", job.ID, next), Outcome: governance.OutcomeProductive}, nil
		case "update":
			id, _ := stringArg(args, "job_id")
			updated, err := store.Mutate(strings.TrimSpace(id), func(job *cron.Job) error {
				return applyCronUpdate(runner, job, args)
			})
			if err != nil {
				return governance.Result{}, err
			}
			next := "-"
			if updated.NextRunAt != nil {
				next = *updated.NextRunAt
			}
			return governance.Result{Content: fmt.Sprintf("job_id: %s\nnext_run_at: %s", updated.ID, next), Outcome: governance.OutcomeProductive}, nil
		case "pause":
			id, _ := stringArg(args, "job_id")
			reason, _ := stringArg(args, "paused_reason")
			updated, err := store.Mutate(strings.TrimSpace(id), func(job *cron.Job) error {
				now := time.Now().UTC()
				job.State = cron.StatePaused
				job.Enabled = false
				at := now.Format(time.RFC3339)
				job.PausedAt = &at
				job.PausedReason = strings.TrimSpace(reason)
				job.NextRunAt = nil
				return nil
			})
			if err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: fmt.Sprintf("job_id: %s paused", updated.ID), Outcome: governance.OutcomeProductive}, nil
		case "resume":
			id, _ := stringArg(args, "job_id")
			updated, err := store.Mutate(strings.TrimSpace(id), func(job *cron.Job) error {
				now := time.Now().UTC()
				job.State = cron.StateScheduled
				job.Enabled = true
				job.PausedAt = nil
				job.PausedReason = ""
				if job.Schedule.IsRepeat() {
					if advance, err := cron.ScheduleNext(job.Schedule, now); err == nil {
						at := advance.Format(time.RFC3339)
						job.NextRunAt = &at
					} else {
						job.NextRunAt = nil
					}
				} else if strings.TrimSpace(job.Schedule.RunAt) != "" {
					if runAt, err := time.Parse(time.RFC3339, strings.TrimSpace(job.Schedule.RunAt)); err == nil && runAt.After(now) {
						at := runAt.UTC().Format(time.RFC3339)
						job.NextRunAt = &at
					} else {
						job.NextRunAt = nil
					}
				}
				return nil
			})
			if err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: fmt.Sprintf("job_id: %s resumed", updated.ID), Outcome: governance.OutcomeProductive}, nil
		case "remove":
			id, _ := stringArg(args, "job_id")
			if err := store.Remove(strings.TrimSpace(id)); err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: "job removed", Outcome: governance.OutcomeProductive}, nil
		case "run":
			id, _ := stringArg(args, "job_id")
			runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			ex, out, err := store.RunNow(runCtx, runner, strings.TrimSpace(id))
			if err != nil {
				return governance.Result{}, err
			}
			if out != "" {
				return governance.Result{Content: outputpkg.Clean(out), Outcome: governance.OutcomeProductive}, nil
			}
			return governance.Result{Content: fmt.Sprintf("status: %s delivered=%t", ex.Status, ex.Delivered), Outcome: governance.OutcomeProductive}, nil
		default:
			return governance.Result{}, errors.New("action must be create|list|update|pause|resume|remove|run")
		}
	}
}

func decodeCronInput(ctx context.Context, args map[string]interface{}) (cron.Job, error) {
	var job cron.Job
	rawSchedule, _ := stringArg(args, "schedule")
	if strings.TrimSpace(rawSchedule) == "" {
		return cron.Job{}, errors.New("schedule is required")
	}
	sched, _, err := cron.ParseSchedule(rawSchedule, time.Now().UTC())
	if err != nil {
		return cron.Job{}, err
	}
	job.Schedule = sched
	job.Name, _ = stringArg(args, "name")
	job.Prompt, _ = stringArg(args, "prompt")
	job.Script, _ = stringArg(args, "script")
	job.Script = strings.TrimSpace(job.Script)
	if v, err := boolArg(args, "no_agent", false); err == nil {
		job.NoAgent = v
	}
	deliver, err := decodeDeliverInput(ctx, args, nil)
	if err != nil {
		return cron.Job{}, err
	}
	job.Deliver = deliver
	if _, ok := args["repeat_times"]; ok {
		if v, err := intArg(args, "repeat_times", 0); err == nil {
			if v > 0 {
				n := v
				job.Repeat.Times = &n
			}
		}
	}
	if v, err := boolArg(args, "paused", false); err == nil && v {
		job.State = cron.StatePaused
	}
	job.PausedReason, _ = stringArg(args, "paused_reason")
	job.Skills = stringSlice(args["skills"])
	job.EnabledToolsets = stringSlice(args["enabled_toolsets"])
	if raw, ok := args["mcp_servers"]; ok {
		items, isList := raw.([]interface{})
		if !isList {
			return cron.Job{}, errors.New("mcp_servers must be an array of server names")
		}
		servers := stringSlice(items)
		if err := cron.ValidateMCPServers(servers); err != nil {
			return cron.Job{}, err
		}
		job.MCPServers = servers
	}
	return job, nil
}

// mcpDisplay renders the allowlist for list output: all, none, or names.
func mcpDisplay(servers []string) string {
	if servers == nil {
		return "all"
	}
	if len(servers) == 0 {
		return "none"
	}
	return strings.Join(servers, ",")
}

// decodeDeliverInput resolves the deliver target: explicit fields win, and
// an omitted target defaults to the creating chat from request metadata.
func decodeDeliverInput(ctx context.Context, args map[string]interface{}, base *cron.Job) (cron.Deliver, error) {
	var platform, chatID, userID string
	hasPlatform := false
	hasChat := false
	hasUser := false
	if _, ok := args["deliver_platform"]; ok {
		hasPlatform = true
		platform, _ = stringArg(args, "deliver_platform")
	}
	if _, ok := args["deliver_chat_id"]; ok {
		hasChat = true
		chatID, _ = stringArg(args, "deliver_chat_id")
	}
	if _, ok := args["deliver_user_id"]; ok {
		hasUser = true
		userID, _ = stringArg(args, "deliver_user_id")
	}
	if !hasPlatform && !hasChat && !hasUser {
		if base != nil {
			return base.Deliver, nil
		}
		return deliverFromContext(ctx)
	}
	out := cron.Deliver{}
	if base != nil {
		out = base.Deliver
	}
	if hasPlatform {
		out.Platform = strings.TrimSpace(platform)
	}
	if hasChat {
		out.ChatID = strings.TrimSpace(chatID)
	}
	if hasUser {
		out.UserID = strings.TrimSpace(userID)
	}
	if base == nil && !hasPlatform && !hasChat {
		return deliverFromContext(ctx)
	}
	return cron.NormalizeDeliver(out)
}

// deliverFromContext defaults the target to the creating chat: the session
// key carries platform, chat, and user (<platform>:<chat>:<user>).
func deliverFromContext(ctx context.Context) (cron.Deliver, error) {
	meta := requestctx.MetadataFromContext(ctx)
	session := strings.TrimSpace(meta.SessionID)
	parts := strings.SplitN(session, ":", 3)
	if len(parts) == 3 && parts[0] != "" && parts[1] != "" {
		return cron.NormalizeDeliver(cron.Deliver{Platform: parts[0], ChatID: parts[1], UserID: parts[2]})
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return cron.NormalizeDeliver(cron.Deliver{Platform: parts[0], ChatID: parts[1]})
	}
	return cron.Deliver{}, errors.New("deliver target is required: pass deliver_platform and deliver_chat_id")
}

func applyCronUpdate(runner *cron.Runner, job *cron.Job, args map[string]interface{}) error {
	// Core -> schedule -> run fields; first failure wins.
	now := time.Now().UTC()
	if raw, ok := args["schedule"]; ok {
		schedule, _ := raw.(string)
		sched, _, err := cron.ParseSchedule(schedule, now)
		if err != nil {
			return err
		}
		job.Schedule = sched
		if job.State != cron.StatePaused {
			if sched.IsRepeat() {
				if advance, err := cron.ScheduleNext(sched, now); err == nil {
					at := advance.Format(time.RFC3339)
					job.NextRunAt = &at
				} else {
					job.NextRunAt = nil
				}
			} else {
				job.NextRunAt = nil
			}
		}
	}
	if raw, ok := args["prompt"]; ok {
		prompt, _ := raw.(string)
		if err := cron.ScanPrompt(prompt); err != nil {
			return err
		}
		job.Prompt = strings.TrimSpace(prompt)
	}
	if raw, ok := args["name"]; ok {
		if name, ok := raw.(string); ok {
			job.Name = strings.TrimSpace(name)
		}
	}
	if _, present := args["repeat_times"]; present {
		if v, err := intArg(args, "repeat_times", 0); err != nil || v < 0 {
			return errors.New("repeat_times must be an integer >= 0")
		} else if v == 0 {
			job.Repeat.Times = nil
		} else {
			n := v
			job.Repeat.Times = &n
		}
	}
	deliver, err := decodeDeliverInput(context.Background(), args, job)
	if err != nil {
		return err
	}
	if _, ok := args["deliver_platform"]; ok {
		job.Deliver = deliver
	} else if _, ok := args["deliver_chat_id"]; ok {
		job.Deliver = deliver
	} else if _, ok := args["deliver_user_id"]; ok {
		job.Deliver = deliver
	}
	if raw, ok := args["skills"]; ok {
		job.Skills = stringSlice(raw)
	}
	if raw, ok := args["enabled_toolsets"]; ok {
		refs := stringSlice(raw)
		for _, toolset := range refs {
			ok := false
			for _, allowed := range cron.AllowedToolsets {
				if toolset == allowed {
					ok = true
					break
				}
			}
			if !ok {
				return errors.New("unknown toolset " + toolset)
			}
		}
		job.EnabledToolsets = refs
	}
	if raw, ok := args["mcp_servers"]; ok {
		items, isList := raw.([]interface{})
		if !isList {
			return errors.New("mcp_servers must be an array of server names")
		}
		servers := stringSlice(items)
		if err := cron.ValidateMCPServers(servers); err != nil {
			return err
		}
		job.MCPServers = servers
	}
	if raw, ok := args["script"]; ok {
		script, _ := raw.(string)
		if strings.TrimSpace(script) != "" && runner != nil {
			if _, err := runner.ResolveScript(script); err != nil {
				return errors.New("script path must exist inside the scripts directory")
			}
		}
		job.Script = strings.TrimSpace(script)
	}
	if raw, ok := args["no_agent"]; ok {
		if v, ok := raw.(bool); ok {
			job.NoAgent = v
		}
	}
	if job.NoAgent && job.Script == "" {
		return errors.New("no_agent requires script")
	}
	if job.Prompt == "" && !job.NoAgent {
		return errors.New("prompt is required")
	}
	if job.Prompt != "" {
		if err := cron.ScanPrompt(job.Prompt); err != nil {
			return err
		}
	}
	return nil
}

func stringSlice(raw interface{}) []string {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
