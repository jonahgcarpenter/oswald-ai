package cronjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	outputpkg "github.com/jonahgcarpenter/oswald-ai/internal/tools/output"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

// cronValidateTimeout bounds deliver-target liveness probes at
// create/update time.
const cronValidateTimeout = 30 * time.Second

// modelValidateTimeout bounds model-existence listing probes at create/update
// time.
const modelValidateTimeout = 8 * time.Second

// NewCronjobManageHandler returns the scheduler handler. Validators is the
// per-platform deliver-target liveness check (nil skips liveness, which
// hermetic unit tests use; production always wires it). modelValidator checks
// explicit model references against live provider state (nil skips the live
// check; production always wires it); syntax is always enforced.
func NewCronjobManageHandler(ws *workspace.Workspace, store *cron.Store, validators map[string]cron.TargetValidator, modelValidator cron.ModelValidator) func(context.Context, map[string]interface{}) (governance.Result, error) {
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
				fmt.Fprintf(&sb, "%s %s %s state=%s next=%s mcp=%s model=%s status=%s\n", job.ID, outputpkg.Clean(job.Name), outputpkg.Clean(job.Schedule.Display), job.State, outputpkg.FormatModelTimestamp(next), mcpDisplay(job.MCPServers), modelDisplay(job.Model), job.LastStatus)
			}
			return governance.Result{Content: strings.TrimSuffix(sb.String(), "\n"), Outcome: governance.OutcomeProductive}, nil
		case "create":
			if _, ok := args["repeat_times"]; ok {
				if _, err := intArg(args, "repeat_times", 0); err != nil {
					return governance.Result{}, errors.New("repeat_times must be an integer >= 0")
				}
			}
			input, err := decodeCronInput(ctx, args, validators, modelValidator)
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
			return governance.Result{Content: fmt.Sprintf("job_id: %s\nnext_run_at: %s", job.ID, outputpkg.FormatModelTimestamp(next)), Outcome: governance.OutcomeProductive}, nil
		case "update":
			id, _ := stringArg(args, "job_id")
			updated, err := store.Mutate(strings.TrimSpace(id), func(job *cron.Job) error {
				return applyCronUpdate(ctx, runner, job, args, validators, modelValidator)
			})
			if err != nil {
				return governance.Result{}, err
			}
			next := "-"
			if updated.NextRunAt != nil {
				next = *updated.NextRunAt
			}
			return governance.Result{Content: fmt.Sprintf("job_id: %s\nnext_run_at: %s", updated.ID, outputpkg.FormatModelTimestamp(next)), Outcome: governance.OutcomeProductive}, nil
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

func decodeCronInput(ctx context.Context, args map[string]interface{}, validators map[string]cron.TargetValidator, modelValidator cron.ModelValidator) (cron.Job, error) {
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
	deliver, err := decodeDeliverInput(ctx, args, nil, validators)
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
	if _, present := args["model"]; present {
		parsed, _, err := parseModelInput(args["model"])
		if err != nil {
			return cron.Job{}, err
		}
		if parsed != nil {
			if err := validateModelLive(ctx, modelValidator, parsed.Provider, parsed.Model); err != nil {
				return cron.Job{}, err
			}
		}
		job.Model = parsed
	}
	if job.NoAgent && job.Model != nil {
		return cron.Job{}, errors.New("model requires agent")
	}
	return job, nil
}

// modelDisplay renders the job model pin for list output: provider/model or
// default when the job runs the profile default.
func modelDisplay(model *cron.JobModel) string {
	if model == nil {
		return "default"
	}
	provider := strings.TrimSpace(model.Provider)
	id := strings.TrimSpace(model.Model)
	if provider == "" || id == "" {
		return "default"
	}
	return outputpkg.Clean(provider + "/" + id)
}

// parseModelInput parses the optional model object. A nil value clears back
// to the profile default. Unknown keys fail closed so typos cannot silently
// pin the wrong transport.
func parseModelInput(raw interface{}) (*cron.JobModel, bool, error) {
	if raw == nil {
		return nil, true, nil
	}
	var fields map[string]interface{}
	switch v := raw.(type) {
	case map[string]interface{}:
		fields = v
	case map[string]string:
		fields = make(map[string]interface{}, len(v))
		for key, val := range v {
			fields[key] = val
		}
	default:
		return nil, false, errors.New("model must be {provider, model} or null")
	}
	for key := range fields {
		if key != "provider" && key != "model" {
			return nil, false, fmt.Errorf("unknown model field %q", key)
		}
	}
	providerRaw, _ := fields["provider"].(string)
	modelRaw, _ := fields["model"].(string)
	if _, ok := fields["provider"]; !ok {
		providerRaw = ""
	}
	if _, ok := fields["model"]; !ok {
		modelRaw = ""
	}
	normalized, err := cron.NormalizeJobModel(providerRaw, modelRaw)
	if err != nil {
		return nil, false, err
	}
	return &normalized, true, nil
}

// validateModelLive checks a normalized reference against live provider
// state. A nil validator skips the check (hermetic unit tests); production
// always wires it. The timeout bounds one listing fetch so a dead provider
// cannot stall job creation.
func validateModelLive(ctx context.Context, validator cron.ModelValidator, provider, model string) error {
	if validator == nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, modelValidateTimeout)
	defer cancel()
	return validator.ValidateModel(callCtx, provider, model)
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
// Explicit targets are liveness-checked against the validators; the
// defaulted live-session target is trusted by construction.
func decodeDeliverInput(ctx context.Context, args map[string]interface{}, base *cron.Job, validators map[string]cron.TargetValidator) (cron.Deliver, error) {
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
	resolved, err := cron.NormalizeDeliver(out)
	if err != nil {
		return cron.Deliver{}, err
	}
	if hasPlatform || hasChat || hasUser {
		if err := validateDeliverTarget(ctx, validators, resolved); err != nil {
			return cron.Deliver{}, err
		}
	}
	return resolved, nil
}

// validateDeliverTarget liveness-checks an explicit deliver target. A nil
// validators map skips the check (hermetic unit tests); a wired map
// requires the platform to be configured and the target to verify.
func validateDeliverTarget(ctx context.Context, validators map[string]cron.TargetValidator, target cron.Deliver) error {
	if validators == nil {
		return nil
	}
	validator, ok := validators[strings.ToLower(strings.TrimSpace(target.Platform))]
	if !ok || validator == nil {
		return fmt.Errorf("deliver platform %q is not configured for delivery in this deployment", target.Platform)
	}
	callCtx, cancel := context.WithTimeout(ctx, cronValidateTimeout)
	defer cancel()
	return validator.ValidateTarget(callCtx, target.ChatID, target.UserID)
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

func applyCronUpdate(ctx context.Context, runner *cron.Runner, job *cron.Job, args map[string]interface{}, validators map[string]cron.TargetValidator, modelValidator cron.ModelValidator) error {
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
			} else if runAt, err := time.Parse(time.RFC3339, strings.TrimSpace(sched.RunAt)); err == nil && runAt.After(now) {
				// One-shots keep their recorded instant so the ticker's
				// ClaimDue sees them; a null next_run_at is skipped.
				at := runAt.UTC().Format(time.RFC3339)
				job.NextRunAt = &at
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
	deliver, err := decodeDeliverInput(ctx, args, job, validators)
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
	if _, present := args["model"]; present {
		parsed, _, err := parseModelInput(args["model"])
		if err != nil {
			return err
		}
		if parsed != nil {
			if err := validateModelLive(ctx, modelValidator, parsed.Provider, parsed.Model); err != nil {
				return err
			}
		}
		job.Model = parsed
	}
	if job.NoAgent && job.Model != nil {
		return errors.New("model requires agent")
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

// The arg helpers below are intentionally local: every tools/* package owns
// its own small copies instead of a shared generic helper package.

func rejectUnknown(args map[string]interface{}, allowed ...string) error {
	keep := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		keep[k] = true
	}
	for k := range args {
		if !keep[k] {
			return fmt.Errorf("unknown argument %q", k)
		}
	}
	return nil
}

func stringArg(args map[string]interface{}, key string) (string, bool) {
	v, ok := args[key].(string)
	return v, ok
}

func intArg(args map[string]interface{}, key string, def int) (int, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return def, nil
	}
	switch v := raw.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		return int(v), nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("%s must be an integer", key)
	}
}

func boolArg(args map[string]interface{}, key string, def bool) (bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return def, nil
	}
	v, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return v, nil
}
