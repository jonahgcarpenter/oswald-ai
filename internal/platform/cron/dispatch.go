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
	// MCPServers scopes the MCP bridge for the turn. Nil exposes every
	// profile server; an empty list exposes none. Passed through verbatim
	// so the nil-vs-empty distinction survives.
	MCPServers []string
}

// AgentResult is the completed agent turn.
type AgentResult struct {
	Text        string
	Attachments []media.OutputAttachment
	Err         error
}

// AgentFunc executes one scheduled agent turn and waits for its result.
type AgentFunc func(ctx context.Context, call AgentCall) (AgentResult, error)

// Sender delivers finished job output to a conversation. DeliverMention
// carries a groupchat mention (address plus header line); senders that tap
// natively may fold it into one message, others bubble the header and thread
// the body under it. DMs never mention and always use Deliver.
type Sender interface {
	Deliver(ctx context.Context, profile, sessionKey, text string, attachments []media.OutputAttachment) error
	DeliverMention(ctx context.Context, profile, sessionKey string, mention Mention, text string, attachments []media.OutputAttachment) error
}

// Mention is a groupchat tap: the transport-native address plus the job
// label. Senders qualify the label per transport (Discord prefixes the native
// mention, iMessage prefixes the profile in its bubble); DMs never mention.
type Mention struct {
	Address string
	Header  string
}

// HasRecipient reports whether the mention names a user. DMs and unset
// targets carry none.
func (m Mention) HasRecipient() bool { return strings.TrimSpace(m.Address) != "" }

// TargetValidator checks a deliver target against live transport state at
// job create/update time. Implementations must be read-only: no messages,
// no channel or conversation creation beyond what the send path itself
// would do.
type TargetValidator interface {
	ValidateTarget(ctx context.Context, chatID, userID string) error
}

// NewGenerationFunc opens a fresh session generation for a job fire.
type NewGenerationFunc func(ctx context.Context, profile, sessionKey string) error

// DispatchTimeout bounds one tick-time agent execution.
const DispatchTimeout = 30 * time.Minute

// MaxPhaseAttempts bounds retries per phase (agent work, then delivery)
// within one tick. Delivery failures count toward failure_streak per
// attempt, so one fully failing tick trips the FailureStreakLimit auto-pause.
const MaxPhaseAttempts = 3

// DeliveryRetryBackoff spaces delivery re-sends within one tick so
// transient transport failures can clear without hammering the backend.
const DeliveryRetryBackoff = 10 * time.Second

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
	// MaxAttempts overrides MaxPhaseAttempts when positive.
	MaxAttempts int
	// RetryBackoff overrides DeliveryRetryBackoff when non-zero.
	// Tests use a nanosecond backoff for speed.
	RetryBackoff time.Duration
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

// maxAttempts bounds retries per phase within one tick.
func (d *Dispatcher) maxAttempts() int {
	if d != nil && d.MaxAttempts > 0 {
		return d.MaxAttempts
	}
	return MaxPhaseAttempts
}

// retryBackoff spaces delivery re-sends within one tick.
func (d *Dispatcher) retryBackoff() time.Duration {
	if d != nil && d.RetryBackoff != 0 {
		return d.RetryBackoff
	}
	return DeliveryRetryBackoff
}

// sleepCtx waits out the backoff or returns false when ctx is done first.
func sleepCtx(ctx context.Context, backoff time.Duration) bool {
	if backoff <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
// Script execution retries on failure; delivery retries only re-send.
func (d *Dispatcher) runScriptDispatch(ctx context.Context, job *Job, started time.Time) (Execution, string, error) {
	var out string
	var runErr error
	for attempt := 1; ; attempt++ {
		out, runErr = runScriptFile(ctx, d.Runner, job.Script, 3600)
		if runErr == nil {
			break
		}
		failed := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "script_error", OutputPreview: preview(out), Delivered: false, DeliveryError: runErr.Error()}
		d.Store.appendLedger(failed)
		d.recordRun(job, failed.Status, failed.DeliveryError, true)
		if attempt >= d.maxAttempts() {
			return failed, "", nil
		}
	}
	status := "completed_script"
	if strings.TrimSpace(out) == "" {
		ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: status, Delivered: true}
		d.Store.appendLedger(ex)
		d.recordRun(job, status, "", false)
		return ex, "", nil
	}
	ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: status, OutputPreview: preview(out)}
	d.deliver(ctx, job, &ex, out, nil)
	d.Store.appendLedger(ex)
	d.recordRun(job, status, ex.DeliveryError, !ex.Delivered)
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
	// Agent work retries on call error/timeout only: misconfiguration
	// (blocked_config above) is deterministic and never retried. Each
	// attempt gets the full bound and its own ledger row.
	var result AgentResult
	var agentErr error
	for attempt := 1; ; attempt++ {
		waitCtx, cancel := context.WithTimeout(ctx, waitBound)
		result, agentErr = d.Agent(waitCtx, AgentCall{
			Profile: d.Profile, SessionKey: session, DisplayName: displayName(job),
			Prompt: prompt, SkipMemory: true, EnabledToolsets: job.EnabledToolsets, MCPServers: job.MCPServers,
		})
		cancel()
		if agentErr == nil {
			break
		}
		status := "error"
		if errors.Is(agentErr, context.DeadlineExceeded) {
			status = "timeout"
		}
		ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: status, OutputPreview: preview(result.Text), Delivered: false, DeliveryError: agentErr.Error()}
		d.Store.appendLedger(ex)
		d.recordRun(job, status, ex.DeliveryError, true)
		if attempt >= d.maxAttempts() {
			return ex, result.Text, nil
		}
	}
	text := strings.TrimSpace(result.Text)
	if strings.HasPrefix(text, "[SILENT]") {
		ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "completed_silent", OutputPreview: preview(text), Delivered: true}
		d.Store.appendLedger(ex)
		d.recordRun(job, ex.Status, "", false)
		return ex, text, nil
	}
	ex := Execution{JobID: job.ID, Instant: job.SlotUnix, StartedUnix: started.Unix(), FinishedUnix: d.now().Unix(), Status: "agent_completed", OutputPreview: preview(text)}
	d.deliver(ctx, job, &ex, text, result.Attachments)
	d.Store.appendLedger(ex)
	d.recordRun(job, ex.Status, ex.DeliveryError, !ex.Delivered)
	return ex, text, nil
}

func displayName(job *Job) string {
	if strings.TrimSpace(job.Name) != "" {
		return job.Name
	}
	return "scheduled job " + job.ID
}

// deliver routes finished output to the job's concrete deliver target. A
// groupchat user_id is mentioned on the response; DMs ignore it. Delivery
// retries only re-send: the payload is never regenerated. Intermediate
// failures are recorded immediately (ledger row plus streak accounting) so
// a fully failing tick trips the auto-pause limit; the caller records the
// final outcome. Only failures rename the status to delivery_failed.
func (d *Dispatcher) deliver(ctx context.Context, job *Job, ex *Execution, text string, attachments []media.OutputAttachment) {
	sender, sessionKey, mention, err := d.resolveTarget(job)
	if err != nil {
		ex.Delivered = false
		ex.DeliveryError = err.Error()
		ex.Status = "delivery_failed"
		return
	}
	for attempt := 1; ; attempt++ {
		sendCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := deliverMentioned(sendCtx, sender, d.Profile, sessionKey, mention, wrapDelivery(job, text), attachments)
		cancel()
		if err == nil {
			ex.Delivered = true
			return
		}
		ex.Delivered = false
		ex.DeliveryError = err.Error()
		ex.Status = "delivery_failed"
		if attempt >= d.maxAttempts() {
			return
		}
		failed := Execution{JobID: job.ID, Instant: ex.Instant, StartedUnix: ex.StartedUnix, FinishedUnix: d.now().Unix(), Status: "delivery_failed", OutputPreview: preview(text), Delivered: false, DeliveryError: err.Error()}
		d.Store.appendLedger(failed)
		d.recordRun(job, failed.Status, failed.DeliveryError, true)
		if !sleepCtx(ctx, d.retryBackoff()) {
			return
		}
	}
}

// deliverMentioned routes through the mention path for named groupchat
// targets and the plain path otherwise (DMs never mention).
func deliverMentioned(ctx context.Context, sender Sender, profile, sessionKey string, mention Mention, text string, attachments []media.OutputAttachment) error {
	if mention.HasRecipient() {
		return sender.DeliverMention(ctx, profile, sessionKey, mention, text, attachments)
	}
	return sender.Deliver(ctx, profile, sessionKey, text, attachments)
}

// resolveTarget maps a job's deliver target onto a live sender, the
// transport session key, and the groupchat mention (empty for DMs).
func (d *Dispatcher) resolveTarget(job *Job) (Sender, string, Mention, error) {
	if job == nil {
		return nil, "", Mention{}, errors.New("cron job is unavailable")
	}
	platform, err := normalizePlatform(job.Deliver.Platform)
	if err != nil {
		return nil, "", Mention{}, err
	}
	chatID := strings.TrimSpace(job.Deliver.ChatID)
	userID := strings.TrimSpace(job.Deliver.UserID)
	if chatID == "" {
		return nil, "", Mention{}, errors.New("deliver chat_id is required")
	}
	sender, ok := d.Senders[platform]
	if !ok || sender == nil {
		return nil, "", Mention{}, errors.New("no live sender for deliver platform " + platform)
	}
	// A chat addressed at the user themselves (or the dm marker) routes as
	// a DM and never mentions.
	if userID != "" && (strings.EqualFold(chatID, "dm") || chatID == userID) {
		return sender, platform + ":dm:" + userID, Mention{}, nil
	}
	sessionKey := platform + ":" + chatID + ":" + userID
	var mention Mention
	if userID != "" {
		mention = Mention{Address: userID, Header: displayName(job)}
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
	_ = deliverMentioned(ctx, sender, d.Profile, sessionKey, mention, text, nil)
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
