package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	outputpkg "github.com/jonahgcarpenter/oswald-ai/internal/tools/output"
)

// CronjobManageDefinition returns the scheduler contract. Model/provider
// overrides are intentionally absent: job identity is user-owned and the
// agent must not set it.
func CronjobManageDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        CronjobManageName,
		Description: "Manage scheduled jobs: create, list, update, pause, resume, remove, or run now. Schedules accept durations (30m), every-phrases (every 2h, every monday 9am), cron (0 9 * * *), RFC3339 one-shots, or in-phrases (in 30m). Prompts must be self-contained: the final response is delivered and cannot ask questions. Use script for stdout-injected work (no_agent=true runs the script as the job, empty output sends nothing); monitor URL/script diff-gates execution with a first-tick baseline. Delivery targets: local, all, origin, bot-chat[:profile], or platform:chat[:thread].",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"action":            {Type: "string", Description: "create|list|update|pause|resume|remove|run"},
			"job_id":            {Type: "string", Description: "Job ID for update/pause/resume/remove/run"},
			"schedule":          {Type: "string", Description: "30m, every 2h, every monday 9am, 0 9 * * *, RFC3339 one-shot, or in 30m"},
			"prompt":            {Type: "string", Description: "Self-contained prompt; the final response is delivered and cannot ask questions"},
			"skills":            {Type: "array", Description: "Skill names available to the run", Items: &llm.ToolParameterProperty{Type: "string"}},
			"script":            {Type: "string", Description: "Workspace script path; stdout is injected (no_agent=true runs it as the job)"},
			"monitor_url":       {Type: "string", Description: "Monitor URL (exclusive with monitor_script)"},
			"monitor_script":    {Type: "string", Description: "Monitor script path (exclusive with monitor_url)"},
			"no_agent":          {Type: "boolean", Description: "Script is the job; stdout verbatim, empty sends nothing", Default: false},
			"deliver":           {Type: "string", Description: "local|all|origin|bot-chat[:profile]|platform:chat[:thread]"},
			"failure_deliver":   {Type: "string", Description: "Delivery target on failure"},
			"context_from":      {Type: "array", Description: "Workspace paths injected as context", Items: &llm.ToolParameterProperty{Type: "string"}},
			"continuity":        {Type: "boolean", Description: "Continue prior run context", Default: false},
			"enabled_toolsets":  {Type: "array", Description: "Toolset allowlist ([] = zero-tool)", Items: &llm.ToolParameterProperty{Type: "string"}},
			"workdir":           {Type: "string", Description: "Job working directory inside the workspace"},
			"attach_to_session": {Type: "string", Description: "Attach execution to a session key"},
			"repeat":            {Type: "boolean", Description: "Catch up missed runs (default true)"},
			"paused":            {Type: "boolean", Description: "Create paused with a reason via paused_reason"},
			"paused_reason":     {Type: "string", Description: "Reason when creating paused"},
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
		if scratch, err := ws.EnsureScratch(); err == nil {
			runner.Scratch = scratch
		}
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
		if err := rejectUnknown(args, "action", "job_id", "schedule", "prompt", "skills", "script", "monitor_url", "monitor_script", "no_agent", "deliver", "failure_deliver", "context_from", "continuity", "enabled_toolsets", "workdir", "attach_to_session", "repeat", "paused", "paused_reason", "model", "provider", "base_url"); err != nil {
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
				return governance.Result{}, errors.New("job list failed")
			}
			if len(jobs) == 0 {
				return governance.Result{Content: "(no jobs)", Outcome: governance.OutcomeUnproductive}, nil
			}
			var sb strings.Builder
			for _, job := range jobs {
				fmt.Fprintf(&sb, "%s %s paused=%t next=%d status=%s\n", job.ID, outputpkg.Clean(job.Schedule), job.Paused, job.NextRunUnix, job.LastStatus)
			}
			return governance.Result{Content: strings.TrimSuffix(sb.String(), "\n"), Outcome: governance.OutcomeProductive}, nil
		case "create":
			job, err := store.Create(time.Now(), runner, decodeCronInput(args))
			if err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: fmt.Sprintf("job_id: %s\nnext_run_at: %d", job.ID, job.NextRunUnix), Outcome: governance.OutcomeProductive}, nil
		case "update":
			id, _ := stringArg(args, "job_id")
			updated, err := store.Mutate(strings.TrimSpace(id), func(job *cron.Job) error {
				return applyCronUpdate(runner, job, args)
			})
			if err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: fmt.Sprintf("job_id: %s\nnext_run_at: %d", updated.ID, updated.NextRunUnix), Outcome: governance.OutcomeProductive}, nil
		case "pause":
			id, _ := stringArg(args, "job_id")
			reason, _ := stringArg(args, "paused_reason")
			updated, err := store.Mutate(strings.TrimSpace(id), func(job *cron.Job) error {
				job.Paused = true
				job.PausedReason = strings.TrimSpace(reason)
				return nil
			})
			if err != nil {
				return governance.Result{}, err
			}
			return governance.Result{Content: fmt.Sprintf("job_id: %s paused", updated.ID), Outcome: governance.OutcomeProductive}, nil
		case "resume":
			id, _ := stringArg(args, "job_id")
			updated, err := store.Mutate(strings.TrimSpace(id), func(job *cron.Job) error {
				job.Paused = false
				job.PausedReason = ""
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

func decodeCronInput(args map[string]interface{}) cron.Job {
	var job cron.Job
	job.Schedule, _ = stringArg(args, "schedule")
	job.Prompt, _ = stringArg(args, "prompt")
	job.Script, _ = stringArg(args, "script")
	job.MonitorURL, _ = stringArg(args, "monitor_url")
	job.MonitorScript, _ = stringArg(args, "monitor_script")
	job.Deliver, _ = stringArg(args, "deliver")
	job.FailureDeliver, _ = stringArg(args, "failure_deliver")
	job.Workdir, _ = stringArg(args, "workdir")
	job.AttachToSession, _ = stringArg(args, "attach_to_session")
	job.PausedReason, _ = stringArg(args, "paused_reason")
	if v, err := boolArg(args, "no_agent", false); err == nil {
		job.NoAgent = v
	}
	if v, err := boolArg(args, "continuity", false); err == nil {
		job.Continuity = v
	}
	if v, err := boolArg(args, "paused", false); err == nil && v {
		job.Paused = true
	}
	if raw, ok := args["repeat"]; ok {
		if v, ok := raw.(bool); ok {
			job.Repeat = &v
		}
	}
	job.Skills = stringSlice(args["skills"])
	job.ContextFrom = stringSlice(args["context_from"])
	job.EnabledToolsets = stringSlice(args["enabled_toolsets"])
	return job
}

func applyCronUpdate(runner *cron.Runner, job *cron.Job, args map[string]interface{}) error {
	// Core -> script -> context -> run fields; first failure wins.
	if raw, ok := args["schedule"]; ok {
		schedule, _ := raw.(string)
		parsed, err := cron.ParseSchedule(schedule, time.Now())
		if err != nil {
			return err
		}
		job.Schedule = strings.TrimSpace(schedule)
		if !parsed.Repeat {
			job.NextRunUnix = 0
		} else {
			job.NextRunUnix = parsed.Next.Unix()
		}
	}
	if raw, ok := args["prompt"]; ok {
		prompt, _ := raw.(string)
		if err := cron.ScanPrompt(prompt); err != nil {
			return err
		}
		job.Prompt = strings.TrimSpace(prompt)
	}
	if raw, ok := args["script"]; ok {
		script, _ := raw.(string)
		if strings.TrimSpace(script) != "" && runner != nil && runner.Resolve != nil {
			if _, err := runner.Resolve(script); err != nil {
				return errors.New("script path must exist inside the working directory")
			}
		}
		job.Script = strings.TrimSpace(script)
	}
	if _, hasURL := args["monitor_url"]; hasURL {
		job.MonitorURL, _ = args["monitor_url"].(string)
		job.MonitorURL = strings.TrimSpace(job.MonitorURL)
	}
	if _, hasScript := args["monitor_script"]; hasScript {
		job.MonitorScript, _ = args["monitor_script"].(string)
		job.MonitorScript = strings.TrimSpace(job.MonitorScript)
	}
	if job.MonitorURL != "" && job.MonitorScript != "" {
		return errors.New("monitor_script and monitor_url are exclusive")
	}
	if raw, ok := args["context_from"]; ok {
		refs := stringSlice(raw)
		if runner != nil && runner.Resolve != nil {
			for _, ref := range refs {
				if _, err := runner.Resolve(ref); err != nil {
					return fmt.Errorf("context_from %q is not accessible", ref)
				}
			}
		}
		job.ContextFrom = refs
	}
	if raw, ok := args["deliver"]; ok {
		deliver, _ := raw.(string)
		if err := cron.ValidateDeliver(deliver); err != nil {
			return err
		}
		job.Deliver = strings.TrimSpace(deliver)
	}
	if raw, ok := args["no_agent"]; ok {
		if v, ok := raw.(bool); ok {
			job.NoAgent = v
		}
	}
	if job.NoAgent && job.Script == "" {
		return errors.New("no_agent requires script")
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
