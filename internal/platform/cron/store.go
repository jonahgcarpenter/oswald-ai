package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// resolveFunc maps a model path to an absolute path inside the profile CWD.
type resolveFunc func(raw string) (string, error)

// dirFunc maps a workdir argument to an existing directory inside the CWD.
type dirFunc func(raw string) (string, error)

// Runner executes job scripts resolved under the profile scripts directory.
type Runner struct {
	Resolve    resolveFunc
	ResolveDir dirFunc
	Root       string
	ScriptsDir string
	Scratch    string
	Profile    string
}

// ResolveScript maps a script name to an absolute path inside the scripts
// directory. Bare names resolve under ScriptsDir (falling back to
// <root>/scripts); absolute, home-relative, and traversal paths fail closed.
func (r *Runner) ResolveScript(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("script path is required")
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "~") || (len(name) >= 2 && name[1] == ':') {
		return "", errors.New("script path must be relative to the scripts directory")
	}
	base := strings.TrimSpace(r.ScriptsDir)
	if base == "" && strings.TrimSpace(r.Root) != "" {
		base = filepath.Join(r.Root, "scripts")
	}
	if strings.TrimSpace(base) == "" {
		return "", errors.New("scripts directory is unavailable")
	}
	abs := filepath.Join(base, filepath.FromSlash(name))
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("script path escapes the scripts directory")
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("script path must exist inside the scripts directory")
	}
	return abs, nil
}

// Create validates and stores a new job.
func (s *Store) Create(now time.Time, r *Runner, input Job) (Job, error) {
	now = now.UTC()
	if strings.TrimSpace(input.Schedule.Kind) == "" {
		return Job{}, errors.New("schedule is required")
	}
	switch input.Schedule.Kind {
	case ScheduleCron, ScheduleInterval, ScheduleOnce:
	default:
		return Job{}, errors.New("unknown schedule kind")
	}
	next, err := ScheduleNext(input.Schedule, now)
	if err != nil {
		return Job{}, err
	}
	if input.Schedule.Kind == ScheduleOnce && !next.After(now) {
		return Job{}, errors.New("one-shot schedule must be in the future")
	}
	deliver, err := NormalizeDeliver(input.Deliver)
	if err != nil {
		return Job{}, err
	}
	job := Job{
		ID:              newJobID(),
		Name:            strings.TrimSpace(input.Name),
		Prompt:          strings.TrimSpace(input.Prompt),
		Skills:          append([]string(nil), input.Skills...),
		Script:          strings.TrimSpace(input.Script),
		NoAgent:         input.NoAgent,
		Schedule:        input.Schedule,
		Repeat:          input.Repeat,
		Deliver:         deliver,
		EnabledToolsets: append([]string(nil), input.EnabledToolsets...),
		Enabled:         true,
		State:           StateScheduled,
		PausedReason:    strings.TrimSpace(input.PausedReason),
		CreatedAt:       utcISO(now),
	}
	if job.Repeat.Times != nil && *job.Repeat.Times < 0 {
		return Job{}, errors.New("repeat times must not be negative")
	}
	if job.Repeat.Times == nil && job.Schedule.Kind == ScheduleOnce {
		one := 1
		job.Repeat.Times = &one
	}
	if strings.TrimSpace(input.State) == StatePaused {
		job.Enabled = false
		job.State = StatePaused
		at := utcISO(now)
		job.PausedAt = &at
		if job.PausedReason == "" {
			job.PausedReason = "Created paused."
		}
	} else {
		at := utcISO(next)
		job.NextRunAt = &at
	}
	if err := s.validate(r, &job); err != nil {
		return Job{}, err
	}
	jobs, err := s.loadJobs()
	if err != nil {
		return Job{}, err
	}
	jobs = append(jobs, job)
	if err := s.saveJobs(jobs); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (s *Store) validate(r *Runner, job *Job) error {
	if utf8.RuneCountInString(job.Name) > 64 {
		return errors.New("name exceeds 64 characters")
	}
	if job.NoAgent && job.Script == "" {
		return errors.New("no_agent requires script")
	}
	if job.Prompt == "" && !job.NoAgent {
		return errors.New("prompt is required")
	}
	if job.Prompt != "" {
		if err := ScanPrompt(job.Prompt); err != nil {
			return err
		}
	}
	if err := ValidateDeliver(job.Deliver); err != nil {
		return err
	}
	if r != nil && job.Script != "" {
		if _, err := r.ResolveScript(job.Script); err != nil {
			return err
		}
	}
	for _, toolset := range job.EnabledToolsets {
		ok := false
		for _, allowed := range AllowedToolsets {
			if toolset == allowed {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("unknown toolset %q", toolset)
		}
	}
	if len(job.Skills) > 32 {
		return errors.New("skills exceed bounds")
	}
	return nil
}

// Resolve finds a job by id, exact name, or unambiguous name substring.
// Ambiguous names error instead of guessing.
func (s *Store) Resolve(ref string) (Job, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Job{}, errors.New("job_id is required")
	}
	jobs, err := s.loadJobs()
	if err != nil {
		return Job{}, err
	}
	for _, job := range jobs {
		if job.ID == ref {
			return job, nil
		}
	}
	var exact *Job
	for i := range jobs {
		if strings.EqualFold(jobs[i].Name, ref) {
			cp := jobs[i]
			exact = &cp
			break
		}
	}
	if exact != nil {
		return *exact, nil
	}
	var matches []Job
	lower := strings.ToLower(ref)
	for _, job := range jobs {
		if job.Name != "" && strings.Contains(strings.ToLower(job.Name), lower) {
			matches = append(matches, job)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return Job{}, fmt.Errorf("ambiguous job name %q (%d matches)", ref, len(matches))
	}
	return Job{}, errors.New("unknown job")
}

// Mutate applies an update function atomically (first failure wins per
// field order enforced by the caller building the updated job).
func (s *Store) Mutate(id string, fn func(*Job) error) (Job, error) {
	job, err := s.Resolve(id)
	if err != nil {
		return Job{}, err
	}
	return s.mutateID(job.ID, fn)
}

func (s *Store) mutateID(id string, fn func(*Job) error) (Job, error) {
	jobs, err := s.loadJobs()
	if err != nil {
		return Job{}, err
	}
	for i := range jobs {
		if jobs[i].ID != id {
			continue
		}
		updated := jobs[i]
		if err := fn(&updated); err != nil {
			return Job{}, err
		}
		jobs[i] = updated
		if err := s.saveJobs(jobs); err != nil {
			return Job{}, err
		}
		return updated, nil
	}
	return Job{}, errors.New("unknown job")
}

// Remove deletes a job.
func (s *Store) Remove(id string) error {
	job, err := s.Resolve(id)
	if err != nil {
		return err
	}
	jobs, err := s.loadJobs()
	if err != nil {
		return err
	}
	kept := jobs[:0]
	for _, candidate := range jobs {
		if candidate.ID == job.ID {
			continue
		}
		kept = append(kept, candidate)
	}
	return s.saveJobs(kept)
}

// RunNow executes one no_agent script job synchronously. Agent-mode jobs
// need a dispatcher; use DispatchRun for those.
func (s *Store) RunNow(ctx context.Context, r *Runner, id string) (Execution, string, error) {
	job, err := s.Resolve(id)
	if err != nil {
		return Execution{}, "", err
	}
	if job.EffectivePaused() {
		return Execution{}, "", errors.New("job is paused")
	}
	if job.Prompt != "" {
		if err := ScanPrompt(job.Prompt); err != nil {
			return Execution{}, "", err
		}
	}
	if !job.NoAgent {
		d := DispatcherFor(s.root)
		if d == nil {
			return Execution{}, "", errors.New("agent dispatch is unavailable")
		}
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return d.RunOne(runCtx, job, 5*time.Minute)
	}
	if !s.TryClaim(job.ID) {
		return Execution{}, "", errors.New("job is already running")
	}
	defer s.Release(job.ID)
	var slotUnix int64
	if next, ok := job.NextRunTime(); ok {
		slotUnix = next.Unix()
	}
	return s.runScriptJob(ctx, r, &job, slotUnix)
}

// runScriptJob runs a no_agent script job inline and records the ledger.
func (s *Store) runScriptJob(ctx context.Context, r *Runner, job *Job, slotUnix int64) (Execution, string, error) {
	started := time.Now().UTC()
	out, err := runScriptFile(ctx, r, job.Script, 3600)
	status := "completed_script"
	if err != nil {
		status = "script_error"
	}
	if strings.TrimSpace(out) == "" {
		ex := Execution{JobID: job.ID, Instant: slotUnix, StartedUnix: started.Unix(), FinishedUnix: time.Now().Unix(), Status: status, Delivered: true}
		s.appendLedger(ex)
		s.finishRun(job.ID, status, "", false)
		return ex, "", nil
	}
	ex := Execution{JobID: job.ID, Instant: slotUnix, StartedUnix: started.Unix(), FinishedUnix: time.Now().Unix(), Status: status, OutputPreview: preview(out), Delivered: false}
	d := DispatcherFor(s.root)
	if d == nil {
		d = &Dispatcher{Store: s, Runner: r}
	}
	d.deliver(job, &ex, out, nil)
	s.appendLedger(ex)
	s.finishRun(job.ID, status, ex.DeliveryError, status == "script_error")
	return ex, out, nil
}

// finishRun records completion: status, delivery error, streak accounting,
// repeat progress, and terminal transitions. It reports whether this run
// newly auto-paused the job on consecutive failures.
func (s *Store) finishRun(id, status, deliveryErr string, failed bool) bool {
	now := time.Now().UTC()
	jobs, err := s.loadJobs()
	if err != nil {
		return false
	}
	autoPaused := false
	for i := range jobs {
		if jobs[i].ID != id {
			continue
		}
		at := utcISO(now)
		jobs[i].LastStatus = status
		jobs[i].LastError = deliveryErr
		jobs[i].LastRunAt = &at
		if failed {
			jobs[i].FailureStreak++
		} else {
			jobs[i].FailureStreak = 0
		}
		if jobs[i].State == StateRunning {
			jobs[i].State = StateScheduled
		}
		if jobs[i].Repeat.Times != nil && *jobs[i].Repeat.Times > 0 {
			jobs[i].Repeat.Completed++
			if jobs[i].Repeat.Completed >= *jobs[i].Repeat.Times {
				jobs[i].State = StateCompleted
				jobs[i].Enabled = false
			}
		}
		if failed && jobs[i].FailureStreak >= FailureStreakLimit && jobs[i].State == StateScheduled {
			jobs[i].State = StatePaused
			jobs[i].Enabled = false
			pausedAt := utcISO(now)
			jobs[i].PausedAt = &pausedAt
			jobs[i].PausedReason = fmt.Sprintf("auto-paused after %d consecutive failures", jobs[i].FailureStreak)
			jobs[i].NextRunAt = nil
			autoPaused = true
		}
		break
	}
	_ = s.saveJobs(jobs)
	return autoPaused
}

// ledgerCompleted reports whether an execution row completed that exact
// scheduled instant. Failed or unknown rows never count.
func (s *Store) ledgerCompleted(jobID string, instant int64) bool {
	if instant <= 0 {
		return false
	}
	dir, err := s.dir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, "executions.jsonl"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry Execution
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.JobID != jobID || entry.Instant != instant {
			continue
		}
		switch entry.Status {
		case "completed_script", "completed_local", "completed_silent", "agent_completed", "script_error":
			return true
		}
	}
	return false
}

func (s *Store) appendLedger(e Execution) {
	dir, err := s.dir()
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "executions.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	data, _ := json.Marshal(e)
	data = append(data, '\n')
	_, _ = f.Write(data)
	f.Close()
	s.pruneLedger(path)
}

// pruneLedger keeps the newest 1000 execution rows.
func (s *Store) pruneLedger(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) <= 1000 {
		return
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines[len(lines)-1000:], "\n")+"\n"), 0600)
}

// Executions returns ledger rows for one job, newest last.
func (s *Store) Executions(jobID string) []Execution {
	dir, err := s.dir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "executions.jsonl"))
	if err != nil {
		return nil
	}
	var out []Execution
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry Execution
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.JobID == jobID {
			out = append(out, entry)
		}
	}
	return out
}

// LastOutput returns the newest execution output preview for continuity.
func (s *Store) LastOutput(jobID string) string {
	entries := s.Executions(jobID)
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].OutputPreview != "" {
			return entries[i].OutputPreview
		}
	}
	return ""
}

// ClaimDue advances due jobs and returns copies for dispatch. Advancement
// lands before any work starts, so a crash mid-run can never refire the
// same slot.
func (s *Store) ClaimDue(now time.Time) []Job {
	now = now.UTC()
	jobs, err := s.loadJobs()
	if err != nil {
		return nil
	}
	s.mu.Lock()
	live := make(map[string]bool, len(s.inFlight))
	for id := range s.inFlight {
		live[id] = true
	}
	s.mu.Unlock()
	changed := false
	var due []Job
	for i := range jobs {
		job := &jobs[i]
		if job.State == StateCompleted {
			continue
		}
		if job.EffectivePaused() {
			continue
		}
		if !job.Enabled {
			continue
		}
		// Restore once: a persisted running state with no live claim means
		// the owner is gone. Requeue without replaying history.
		if job.State == StateRunning && !live[job.ID] {
			job.State = StateScheduled
			if job.NextRunAt == nil && job.Schedule.Kind == ScheduleOnce && strings.TrimSpace(job.Schedule.RunAt) != "" {
				runAt := strings.TrimSpace(job.Schedule.RunAt)
				job.NextRunAt = &runAt
			}
			job.LastStatus = "restored_pending"
			changed = true
		}
		next, ok := job.NextRunTime()
		if !ok || now.Before(next) {
			continue
		}
		slot := next.Unix()
		// Already fired never refires: a completed ledger row for this
		// exact instant suppresses the slot.
		if s.ledgerCompleted(job.ID, slot) {
			if job.Schedule.IsRepeat() {
				if advance, perr := ScheduleNext(job.Schedule, now); perr == nil {
					at := utcISO(advance)
					job.NextRunAt = &at
				} else {
					job.NextRunAt = nil
				}
			} else {
				job.NextRunAt = nil
			}
			job.LastStatus = "skipped_duplicate_slot"
			changed = true
			continue
		}
		if !job.Schedule.IsRepeat() && now.Sub(next) > 2*time.Hour {
			// One-shots past a short grace retire with a diagnostic.
			job.NextRunAt = nil
			job.LastStatus = "retired_missed_oneshot"
			changed = true
			continue
		}
		advance, err := ScheduleNext(job.Schedule, now)
		if err != nil {
			job.LastStatus = "blocked_config"
			job.LastError = "schedule no longer parses"
			changed = true
			continue
		}
		// A due stored one-shot claims its recorded instant even though a
		// fresh parse would reject it as past.
		if !job.Schedule.IsRepeat() {
			job.NextRunAt = nil
		} else {
			at := utcISO(advance)
			job.NextRunAt = &at
		}
		job.State = StateRunning
		job.Enabled = true
		changed = true
		claimed := *job
		claimed.SlotUnix = slot
		due = append(due, claimed)
	}
	if changed {
		_ = s.saveJobs(jobs)
	}
	return due
}

func runScriptFile(ctx context.Context, r *Runner, script string, boundSecs int) (string, error) {
	abs := strings.TrimSpace(script)
	if r != nil {
		resolved, err := r.ResolveScript(script)
		if err != nil {
			return "", err
		}
		abs = resolved
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("script path must exist")
	}
	ext := strings.ToLower(filepath.Ext(abs))
	var cmd *exec.Cmd
	bound, cancel := context.WithTimeout(ctx, time.Duration(boundSecs)*time.Second)
	defer cancel()
	switch ext {
	case ".sh", "":
		cmd = exec.CommandContext(bound, "/bin/sh", abs)
	default:
		cmd = exec.CommandContext(bound, "/bin/sh", abs)
	}
	if r != nil && r.Root != "" {
		cmd.Dir = r.Root
	}
	env := []string{"PATH=/usr/bin:/bin:/usr/local/bin", "LANG=C.UTF-8"}
	if r != nil {
		if r.Scratch != "" {
			ensureScratchDir(r.Root, r.Scratch, r.Profile)
			env = append(env, "TMPDIR="+r.Scratch)
		}
		if r.Root != "" {
			env = append(env, "OSWALD_WORKSPACE="+r.Root, "OSWALD_HOME="+r.Root)
		}
	}
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &boundedWriter{buf: &buf, limit: 64 << 10}
	cmd.Stderr = &boundedWriter{buf: &buf, limit: 64 << 10}
	err = cmd.Run()
	return strings.TrimRight(buf.String(), "\n"), err
}

// ensureScratchDir lazily creates the cron scratch directory on first use.
// Only components at or below root are touched; system ancestors above root
// are never created or chmodded. Failures are silent: the caller still sets
// TMPDIR and lets script execution surface the error.
func logCronRepair(profile, artifact string, before os.FileMode) {
	config.NewLogger(config.LevelWarn).Server("cron").Warn("cron.perms.repaired", "repaired private permissions", config.F("config_source", artifact), config.F("old_mode", int(before.Perm())), config.F("profile", profile))
}

func repairCronDir(path, profile, artifact string) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.Mode().Perm()&0077 == 0 {
		return
	}
	before := info.Mode()
	if err := os.Chmod(path, 0700); err != nil {
		return
	}
	logCronRepair(profile, artifact, before)
}

func repairCronFile(path, profile, artifact string) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.Mode().Perm()&0077 == 0 {
		return
	}
	before := info.Mode()
	if err := os.Chmod(path, 0600); err != nil {
		return
	}
	logCronRepair(profile, artifact, before)
}

// RepairProfile tightens permissions on existing cron material and ensures
// the per-profile scripts directory exists. Missing files stay missing;
// symlinks and foreign-owned paths are left for the fail-closed checks.
func RepairProfile(profileRoot, profile string) {
	if strings.TrimSpace(profileRoot) == "" || strings.TrimSpace(profile) == "" {
		return
	}
	dir := filepath.Join(profileRoot, "cron")
	repairCronDir(dir, profile, "cron_dir")
	repairCronFile(filepath.Join(dir, "jobs.json"), profile, "cron_jobs")
	repairCronFile(filepath.Join(dir, "executions.jsonl"), profile, "cron_executions")
	scripts := filepath.Join(profileRoot, "scripts")
	if err := os.MkdirAll(scripts, 0700); err == nil {
		repairCronDir(scripts, profile, "cron_scripts")
	}
}

func ensureScratchDir(root, scratch, profile string) {
	if scratch == "" || root == "" {
		return
	}
	rel, err := filepath.Rel(root, scratch)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0700); err != nil {
				return
			}
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return
		}
		if current == scratch && info.Mode().Perm()&0077 != 0 {
			before := info.Mode()
			if err := os.Chmod(current, 0700); err != nil {
				return
			}
			logCronRepair(profile, "cron_scratch", before)
		}
	}
}

func preview(s string) string {
	r := []rune(s)
	if len(r) > 2000 {
		return string(r[:2000]) + "..."
	}
	return s
}

type boundedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return w.buf.Write(p)
}
