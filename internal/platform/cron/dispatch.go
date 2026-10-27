package cron

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

// AgentCall is one scheduled agent turn.
type AgentCall struct {
	Profile         string
	SessionKey      string
	DisplayName     string
	Prompt          string
	SkipMemory      bool
	EnabledToolsets []string
}

// AgentResult is the completed agent turn.
type AgentResult struct {
	Text        string
	Attachments []media.OutputAttachment
	Err         error
}

// AgentFunc executes one scheduled agent turn and waits for its result.
type AgentFunc func(ctx context.Context, call AgentCall) (AgentResult, error)

// Sender delivers finished job output to a conversation.
type Sender interface {
	Deliver(ctx context.Context, profile, sessionKey, text string, attachments []media.OutputAttachment) error
}

// NewGenerationFunc opens a fresh session generation for a job fire.
type NewGenerationFunc func(ctx context.Context, profile, sessionKey string) error

// DispatchTimeout bounds one tick-time agent execution.
const DispatchTimeout = 30 * time.Minute

// Dispatcher executes claimed jobs and delivers their output.
type Dispatcher struct {
	Store         *Store
	Runner        *Runner
	Profile       string
	Agent         AgentFunc
	Senders       map[string]Sender
	NewGeneration NewGenerationFunc
	Timeout       time.Duration
	Now           func() time.Time
}

func (d *Dispatcher) now() time.Time {
	if d != nil && d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dispatcher) timeout() time.Duration {
	if d != nil && d.Timeout > 0 {
		return d.Timeout
	}
	return DispatchTimeout
}

var (
	dispatchMu       sync.Mutex
	dispatchRegistry = make(map[string]*Dispatcher)
)

// RegisterDispatcher publishes the live dispatcher for a profile root so
// model-triggered manual runs share the tick path.
func RegisterDispatcher(profileRoot string, d *Dispatcher) {
	abs := strings.TrimSpace(profileRoot)
	if abs != "" {
		if resolved, err := filepath.Abs(abs); err == nil {
			abs = resolved
		}
	}
	dispatchMu.Lock()
	defer dispatchMu.Unlock()
	dispatchRegistry[abs] = d
}

// DispatcherFor returns the live dispatcher for a profile root, if any.
func DispatcherFor(profileRoot string) *Dispatcher {
	abs := strings.TrimSpace(profileRoot)
	if abs != "" {
		if resolved, err := filepath.Abs(abs); err == nil {
			abs = resolved
		}
	}
	dispatchMu.Lock()
	defer dispatchMu.Unlock()
	return dispatchRegistry[abs]
}

// assemblePrompt builds the agent prompt: continuity output, script stdout,
// context file contents, then the job prompt.
func (d *Dispatcher) assemblePrompt(ctx context.Context, job *Job, workdir string) (string, error) {
	var parts []string
	if job.Continuity {
		if last := d.Store.LastOutput(job.ID); last != "" {
			parts = append(parts, "Previous run output:\n"+last)
		}
	}
	var scriptOut string
	if job.Script != "" {
		out, err := runScriptFile(ctx, d.Runner, job.Script, workdir, 3600)
		if err != nil {
			return "", fmt.Errorf("job script failed: %w", err)
		}
		scriptOut = out
		if strings.TrimSpace(out) != "" {
			parts = append(parts, "Script output:\n"+out)
		}
	}
	for i, ref := range job.ContextFrom {
		if i >= 8 {
			parts = append(parts, "(context truncated to 8 files)")
			break
		}
		abs := ref
		if d.Runner != nil && d.Runner.Resolve != nil {
			resolved, err := d.Runner.Resolve(ref)
			if err != nil {
				return "", fmt.Errorf("context_from %q is not accessible", ref)
			}
			abs = resolved
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return "", fmt.Errorf("context_from %q is not accessible", ref)
		}
		if len(data) > 8<<10 {
			parts = append(parts, "Context from "+ref+":\n"+string(data[:8<<10])+"\n(truncated)")
		} else {
			parts = append(parts, "Context from "+ref+":\n"+string(data))
		}
	}
	if job.NoAgent {
		return strings.Join(parts, "\n\n"), nil
	}
	parts = append(parts, job.Prompt)
	_ = scriptOut
	return strings.Join(parts, "\n\n"), nil
}

// RunOne executes one claimed job and records its outcome. waitBound caps
// the agent wait; tick callers pass the dispatch ceiling, manual runs a
// shorter bound.
func (d *Dispatcher) RunOne(ctx context.Context, job Job, waitBound time.Duration) (Execution, string, error) {
	started := d.now()
	if d == nil || d.Store == nil {
		return Execution{}, "", errors.New("cron dispatcher is unavailable")
	}
	if !d.Store.TryClaim(job.ID) {
		return Execution{}, "", errors.New("job is already running")
	}
	defer d.Store.Release(job.ID)
	workdir := scriptWorkdir(d.Runner, &job)
	if skip, err := d.Store.checkMonitor(ctx, d.Runner, &job, workdir); err != nil {
		return Execution{}, "", err
	} else if skip {
		ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "skipped_monitor_unchanged", Delivered: true}
		d.Store.appendLedger(ex)
		return ex, "", nil
	}
	if job.NoAgent {
		return d.runScriptDispatch(ctx, &job, workdir, started)
	}
	return d.runAgentDispatch(ctx, &job, workdir, started, waitBound)
}

// runScriptDispatch executes a no_agent script job and delivers its stdout.
func (d *Dispatcher) runScriptDispatch(ctx context.Context, job *Job, workdir string, started time.Time) (Execution, string, error) {
	out, err := runScriptFile(ctx, d.Runner, job.Script, workdir, 3600)
	status := "completed_script"
	failed := false
	if err != nil {
		status = "script_error"
		failed = true
	}
	if strings.TrimSpace(out) == "" {
		ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: status, Delivered: true}
		d.Store.appendLedger(ex)
		d.Store.finishRun(job.ID, status, "", failed)
		return ex, "", nil
	}
	ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: status, OutputPreview: preview(out)}
	d.deliver(job, &ex, out, nil)
	d.Store.appendLedger(ex)
	d.Store.finishRun(job.ID, status, ex.DeliveryError, failed)
	return ex, out, nil
}

// runAgentDispatch assembles the prompt, opens a fresh generation, runs the
// agent turn, and delivers the result.
func (d *Dispatcher) runAgentDispatch(ctx context.Context, job *Job, workdir string, started time.Time, waitBound time.Duration) (Execution, string, error) {
	if d.Agent == nil {
		ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "agent_queued", Delivered: false, DeliveryError: "no agent dispatcher in this deployment; recorded only"}
		d.Store.appendLedger(ex)
		d.Store.finishRun(job.ID, ex.Status, ex.DeliveryError, false)
		return ex, "", nil
	}
	prompt, err := d.assemblePrompt(ctx, job, workdir)
	if err != nil {
		ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "blocked_config", Delivered: false, DeliveryError: err.Error()}
		d.Store.appendLedger(ex)
		d.Store.finishRun(job.ID, ex.Status, ex.DeliveryError, true)
		return ex, "", nil
	}
	session := strings.TrimSpace(job.AttachToSession)
	if session == "" {
		session = "cron:" + job.ID
	}
	if d.NewGeneration != nil {
		_ = d.NewGeneration(ctx, d.Profile, session)
	}
	if waitBound <= 0 {
		waitBound = d.timeout()
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitBound)
	defer cancel()
	result, err := d.Agent(waitCtx, AgentCall{
		Profile: d.Profile, SessionKey: session, DisplayName: displayName(job),
		Prompt: prompt, SkipMemory: true, EnabledToolsets: job.EnabledToolsets,
	})
	if err != nil {
		failed := true
		status := "error"
		if errors.Is(err, context.DeadlineExceeded) {
			status = "timeout"
		}
		ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: status, OutputPreview: preview(result.Text), Delivered: false, DeliveryError: err.Error()}
		d.Store.appendLedger(ex)
		d.Store.finishRun(job.ID, status, ex.DeliveryError, failed)
		return ex, result.Text, nil
	}
	text := strings.TrimSpace(result.Text)
	if strings.HasPrefix(text, "[SILENT]") {
		ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "completed_silent", OutputPreview: preview(text), Delivered: true}
		d.Store.appendLedger(ex)
		d.Store.finishRun(job.ID, ex.Status, "", false)
		return ex, text, nil
	}
	ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "agent_completed", OutputPreview: preview(text)}
	d.deliver(job, &ex, text, result.Attachments)
	d.Store.appendLedger(ex)
	d.Store.finishRun(job.ID, ex.Status, ex.DeliveryError, false)
	return ex, text, nil
}

func displayName(job *Job) string {
	if strings.TrimSpace(job.Name) != "" {
		return job.Name
	}
	return "scheduled job " + job.ID
}

// deliver routes finished output to the creating chat. Origin is the only
// delivery target; every other value records honestly instead of silently
// skipping. Only failures rename the status to delivery_failed.
func (d *Dispatcher) deliver(job *Job, ex *Execution, text string, attachments []media.OutputAttachment) {
	target := strings.TrimSpace(job.Deliver)
	if target != "" && target != "origin" {
		ex.Delivered = false
		ex.DeliveryError = "delivery target " + target + " is not supported; origin is the only delivery target"
		ex.Status = "delivery_failed"
		return
	}
	if target == "" {
		// Belt and braces: stored jobs always carry a target, but an
		// in-memory job may predate the origin-only default.
		target = "origin"
	}
	if strings.TrimSpace(job.OriginSession) == "" || strings.TrimSpace(job.OriginGateway) == "" {
		ex.Delivered = false
		ex.DeliveryError = "unknown origin — recreate the job from the target chat"
		ex.Status = "delivery_failed"
		return
	}
	sender, ok := d.Senders[strings.TrimSpace(job.OriginGateway)]
	if !ok || sender == nil {
		ex.Delivered = false
		ex.DeliveryError = "no live sender for origin gateway " + strings.TrimSpace(job.OriginGateway)
		ex.RegisteredSenders = sortedSenderNames(d.Senders)
		ex.Status = "delivery_failed"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := sender.Deliver(ctx, d.Profile, strings.TrimSpace(job.OriginSession), wrapDelivery(job, text), attachments); err != nil {
		ex.Delivered = false
		ex.DeliveryError = err.Error()
		ex.Status = "delivery_failed"
		return
	}
	ex.Delivered = true
}

// sortedSenderNames lists the registered cron sender gateways in stable
// order for diagnostics.
func sortedSenderNames(senders map[string]Sender) []string {
	names := make([]string, 0, len(senders))
	for name, sender := range senders {
		if sender == nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// wrapDelivery bounds delivered text. The agent response goes out
// verbatim: no job header, no footer.
func wrapDelivery(job *Job, text string) string {
	_ = job
	if utf8.RuneCountInString(text) > 6000 {
		return string([]rune(text)[:6000]) + "\n[truncated]"
	}
	return text
}

// RunDue executes claimed jobs through a bounded pool.
func (d *Dispatcher) RunDue(ctx context.Context, jobs []Job) []Execution {
	const maxParallel = 4
	sem := make(chan struct{}, maxParallel)
	var mu sync.Mutex
	var out []Execution
	var wg sync.WaitGroup
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ex, _, _ := d.RunOne(ctx, job, 0)
			mu.Lock()
			out = append(out, ex)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}
