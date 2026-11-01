package cron

import (
	"context"
	"errors"
	"fmt"
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

// assemblePrompt builds the agent prompt: script stdout, then the job prompt.
func (d *Dispatcher) assemblePrompt(ctx context.Context, job *Job) (string, error) {
	var parts []string
	if job.Script != "" {
		out, err := runScriptFile(ctx, d.Runner, job.Script, 3600)
		if err != nil {
			return "", fmt.Errorf("job script failed: %w", err)
		}
		if strings.TrimSpace(out) != "" {
			parts = append(parts, "Script output:\n"+out)
		}
	}
	if job.NoAgent {
		return strings.Join(parts, "\n\n"), nil
	}
	parts = append(parts, job.Prompt)
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
	if job.NoAgent {
		return d.runScriptDispatch(ctx, &job, started)
	}
	return d.runAgentDispatch(ctx, &job, started, waitBound)
}

// recordRun persists the outcome and alerts the deliver target when the run
// newly auto-pauses the job on consecutive failures.
func (d *Dispatcher) recordRun(job *Job, status, deliveryErr string, failed bool) {
	if d == nil || d.Store == nil || job == nil {
		return
	}
	if d.Store.finishRun(job.ID, status, deliveryErr, failed) {
		d.alertAutoPause(job)
	}
}

// runScriptDispatch executes a no_agent script job and delivers its stdout.
func (d *Dispatcher) runScriptDispatch(ctx context.Context, job *Job, started time.Time) (Execution, string, error) {
	out, err := runScriptFile(ctx, d.Runner, job.Script, 3600)
	status := "completed_script"
	failed := false
	if err != nil {
		status = "script_error"
		failed = true
	}
	if strings.TrimSpace(out) == "" {
		ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: status, Delivered: true}
		d.Store.appendLedger(ex)
		d.recordRun(job, status, "", failed)
		return ex, "", nil
	}
	ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: status, OutputPreview: preview(out)}
	d.deliver(job, &ex, out, nil)
	d.Store.appendLedger(ex)
	d.recordRun(job, status, ex.DeliveryError, failed)
	return ex, out, nil
}

// runAgentDispatch assembles the prompt, opens a fresh generation, runs the
// agent turn, and delivers the result.
func (d *Dispatcher) runAgentDispatch(ctx context.Context, job *Job, started time.Time, waitBound time.Duration) (Execution, string, error) {
	if d.Agent == nil {
		ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "agent_queued", Delivered: false, DeliveryError: "no agent dispatcher in this deployment; recorded only"}
		d.Store.appendLedger(ex)
		d.recordRun(job, ex.Status, ex.DeliveryError, false)
		return ex, "", nil
	}
	prompt, err := d.assemblePrompt(ctx, job)
	if err != nil {
		ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "blocked_config", Delivered: false, DeliveryError: err.Error()}
		d.Store.appendLedger(ex)
		d.recordRun(job, ex.Status, ex.DeliveryError, true)
		return ex, "", nil
	}
	session := "cron:" + job.ID
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
		ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: status, OutputPreview: preview(result.Text), Delivered: false, DeliveryError: err.Error()}
		d.Store.appendLedger(ex)
		d.recordRun(job, status, ex.DeliveryError, failed)
		return ex, result.Text, nil
	}
	text := strings.TrimSpace(result.Text)
	if strings.HasPrefix(text, "[SILENT]") {
		ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "completed_silent", OutputPreview: preview(text), Delivered: true}
		d.Store.appendLedger(ex)
		d.recordRun(job, ex.Status, "", false)
		return ex, text, nil
	}
	ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "agent_completed", OutputPreview: preview(text)}
	d.deliver(job, &ex, text, result.Attachments)
	d.Store.appendLedger(ex)
	d.recordRun(job, ex.Status, ex.DeliveryError, false)
	return ex, text, nil
}

func displayName(job *Job) string {
	if strings.TrimSpace(job.Name) != "" {
		return job.Name
	}
	return "scheduled job " + job.ID
}

// deliver routes finished output to the job's concrete deliver target. A
// groupchat user_id is mentioned on the response; DMs ignore it. Only
// failures rename the status to delivery_failed.
func (d *Dispatcher) deliver(job *Job, ex *Execution, text string, attachments []media.OutputAttachment) {
	sender, sessionKey, mention, err := d.resolveTarget(job)
	if err != nil {
		ex.Delivered = false
		ex.DeliveryError = err.Error()
		ex.Status = "delivery_failed"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := sender.Deliver(ctx, d.Profile, sessionKey, mention+wrapDelivery(job, text), attachments); err != nil {
		ex.Delivered = false
		ex.DeliveryError = err.Error()
		ex.Status = "delivery_failed"
		return
	}
	ex.Delivered = true
}

// resolveTarget maps a job's deliver target onto a live sender, the
// transport session key, and the groupchat mention prefix (empty for DMs).
func (d *Dispatcher) resolveTarget(job *Job) (Sender, string, string, error) {
	if job == nil {
		return nil, "", "", errors.New("cron job is unavailable")
	}
	platform, err := normalizePlatform(job.Deliver.Platform)
	if err != nil {
		return nil, "", "", err
	}
	chatID := strings.TrimSpace(job.Deliver.ChatID)
	userID := strings.TrimSpace(job.Deliver.UserID)
	if chatID == "" {
		return nil, "", "", errors.New("deliver chat_id is required")
	}
	sender, ok := d.Senders[platform]
	if !ok || sender == nil {
		return nil, "", "", errors.New("no live sender for deliver platform " + platform)
	}
	// A chat addressed at the user themselves (or the dm marker) routes as
	// a DM and never mentions.
	if userID != "" && (strings.EqualFold(chatID, "dm") || chatID == userID) {
		return sender, platform + ":dm:" + userID, "", nil
	}
	sessionKey := platform + ":" + chatID + ":" + userID
	mention := ""
	if userID != "" {
		if platform == DeliverDiscord {
			mention = "<@" + userID + "> "
		} else {
			mention = "@" + userID + " "
		}
	}
	return sender, sessionKey, mention, nil
}

// alertAutoPause notifies the deliver target that the job just auto-paused
// on consecutive failures. Best-effort: delivery problems are dropped, never
// logged with job contents.
func (d *Dispatcher) alertAutoPause(job *Job) {
	if d == nil || job == nil {
		return
	}
	updated, err := d.Store.Resolve(job.ID)
	if err != nil {
		return
	}
	if updated.State != StatePaused {
		return
	}
	sender, sessionKey, mention, err := d.resolveTarget(&updated)
	if err != nil {
		return
	}
	text := fmt.Sprintf("Cron job '%s' auto-paused after %d consecutive failures: %s",
		displayName(&updated), updated.FailureStreak, strings.TrimSpace(updated.LastError))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = sender.Deliver(ctx, d.Profile, sessionKey, mention+text, nil)
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
