package cron

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// resolveFunc maps a model path to an absolute path inside the profile CWD.
type resolveFunc func(raw string) (string, error)

// dirFunc maps a workdir argument to an existing directory inside the CWD.
type dirFunc func(raw string) (string, error)

// Runner executes job scripts and resolves workspace paths.
type Runner struct {
	Resolve    resolveFunc
	ResolveDir dirFunc
	Root       string
	Scratch    string
}

// Create validates and stores a new job.
func (s *Store) Create(now time.Time, r *Runner, input Job) (Job, error) {
	if strings.TrimSpace(input.Schedule) == "" {
		return Job{}, errors.New("schedule is required")
	}
	parsed, err := ParseSchedule(input.Schedule, now)
	if err != nil {
		return Job{}, err
	}
	job := Job{
		ID:              newJobID(),
		Name:            strings.TrimSpace(input.Name),
		Schedule:        strings.TrimSpace(input.Schedule),
		Prompt:          strings.TrimSpace(input.Prompt),
		Skills:          append([]string(nil), input.Skills...),
		Script:          strings.TrimSpace(input.Script),
		MonitorURL:      strings.TrimSpace(input.MonitorURL),
		MonitorScript:   strings.TrimSpace(input.MonitorScript),
		NoAgent:         input.NoAgent,
		Deliver:         strings.TrimSpace(input.Deliver),
		FailureDeliver:  strings.TrimSpace(input.FailureDeliver),
		ContextFrom:     append([]string(nil), input.ContextFrom...),
		Continuity:      input.Continuity,
		EnabledToolsets: append([]string(nil), input.EnabledToolsets...),
		Workdir:         strings.TrimSpace(input.Workdir),
		AttachToSession: strings.TrimSpace(input.AttachToSession),
		Repeat:          input.Repeat,
		CatchUp:         input.CatchUp,
		State:           StateScheduled,
		OriginGateway:   strings.TrimSpace(input.OriginGateway),
		OriginSession:   strings.TrimSpace(input.OriginSession),
		NextRunUnix:     parsed.Next.Unix(),
		CreatedUnix:     now.Unix(),
		UpdatedUnix:     now.Unix(),
	}
	if job.Repeat.Times < 0 {
		return Job{}, errors.New("repeat times must not be negative")
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
	if job.MonitorURL != "" && job.MonitorScript != "" {
		return errors.New("monitor_script and monitor_url are exclusive")
	}
	if err := ValidateDeliver(job.Deliver); err != nil {
		return err
	}
	if err := ValidateDeliver(job.FailureDeliver); err != nil {
		return err
	}
	if job.OriginSession != "" && job.OriginGateway == "" {
		return errors.New("origin session requires an origin gateway")
	}
	if r != nil {
		if job.Script != "" {
			if _, err := r.Resolve(job.Script); err != nil {
				return errors.New("script path must exist inside the working directory")
			}
		}
		if job.MonitorScript != "" {
			if _, err := r.Resolve(job.MonitorScript); err != nil {
				return errors.New("monitor script must exist inside the working directory")
			}
		}
		for _, ref := range job.ContextFrom {
			if _, err := r.Resolve(ref); err != nil {
				return fmt.Errorf("context_from %q is not accessible", ref)
			}
		}
		if job.Workdir != "" {
			if _, err := r.ResolveDir(job.Workdir); err != nil {
				return errors.New("workdir is not accessible")
			}
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
	if len(job.Skills) > 32 || len(job.ContextFrom) > 32 {
		return errors.New("skills/context_from exceed bounds")
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
		updated.Paused = updated.State == StatePaused
		updated.UpdatedUnix = time.Now().Unix()
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
	return s.runScriptJob(ctx, r, &job)
}

// runScriptJob runs a no_agent script job inline and records the ledger.
func (s *Store) runScriptJob(ctx context.Context, r *Runner, job *Job) (Execution, string, error) {
	started := time.Now()
	workdir := scriptWorkdir(r, job)
	skip, skipErr := s.checkMonitor(ctx, r, job, workdir)
	if skipErr != nil {
		return Execution{}, "", skipErr
	}
	if skip {
		ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: time.Now().Unix(), Status: "skipped_monitor_unchanged", Delivered: true}
		s.appendLedger(ex)
		return ex, "", nil
	}
	out, err := runScriptFile(ctx, r, job.Script, workdir, 3600)
	status := "completed_script"
	if err != nil {
		status = "script_error"
	}
	if strings.TrimSpace(out) == "" {
		ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: time.Now().Unix(), Status: status, Delivered: true}
		s.appendLedger(ex)
		s.finishRun(job.ID, status, "", false)
		return ex, "", nil
	}
	ex := Execution{JobID: job.ID, Instant: job.LastInstant, StartedUnix: started.Unix(), FinishedUnix: time.Now().Unix(), Status: status, OutputPreview: preview(out), Delivered: false}
	d := DispatcherFor(s.root)
	if d == nil {
		d = &Dispatcher{Store: s, Runner: r}
	}
	d.deliver(job, &ex, out)
	s.appendLedger(ex)
	s.finishRun(job.ID, status, ex.DeliveryError, status == "script_error")
	return ex, out, nil
}

func scriptWorkdir(r *Runner, job *Job) string {
	workdir := ""
	if r != nil {
		workdir = r.Root
		if job.Workdir != "" && r.ResolveDir != nil {
			if resolved, err := r.ResolveDir(job.Workdir); err == nil {
				workdir = resolved
			}
		}
	}
	return workdir
}

// checkMonitor runs the monitor diff gate. First tick establishes the
// baseline. True means unchanged since baseline: skip this run.
func (s *Store) checkMonitor(ctx context.Context, r *Runner, job *Job, workdir string) (bool, error) {
	if job.MonitorScript == "" || r == nil {
		return false, nil
	}
	bound, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	out, merr := runScriptFile(bound, r, job.MonitorScript, workdir, 120)
	if merr != nil {
		return false, nil
	}
	hash := hashString(out)
	if job.BaselineHash != "" && hash == job.BaselineHash {
		return true, nil
	}
	s.persistBaseline(job.ID, hash)
	return false, nil
}

// finishRun records completion: status, delivery error, failure accounting,
// repeat progress, and terminal transitions.
func (s *Store) finishRun(id, status, deliveryErr string, failed bool) {
	jobs, err := s.loadJobs()
	if err != nil {
		return
	}
	for i := range jobs {
		if jobs[i].ID != id {
			continue
		}
		jobs[i].LastStatus = status
		jobs[i].LastError = deliveryErr
		jobs[i].LastRunUnix = time.Now().Unix()
		jobs[i].UpdatedUnix = jobs[i].LastRunUnix
		jobs[i].Pending = PendingSlot{}
		if failed {
			jobs[i].FailureCount++
		}
		if jobs[i].State == StateRunning {
			jobs[i].State = StateScheduled
		}
		if jobs[i].Repeat.Times > 0 {
			jobs[i].Repeat.Completed++
			if jobs[i].Repeat.Completed >= jobs[i].Repeat.Times {
				jobs[i].State = StateCompleted
			}
		}
		jobs[i].Paused = jobs[i].State == StatePaused
		break
	}
	_ = s.saveJobs(jobs)
}

func (s *Store) persistBaseline(id, hash string) {
	jobs, err := s.loadJobs()
	if err != nil {
		return
	}
	for i := range jobs {
		if jobs[i].ID == id {
			jobs[i].BaselineHash = hash
			break
		}
	}
	_ = s.saveJobs(jobs)
}

// deliverLocal saves output to the per-profile output directory for local
// delivery targets. True means it handled the target.
func (s *Store) deliverLocal(r *Runner, job *Job, output string) (bool, error) {
	target := strings.TrimSpace(job.Deliver)
	if target != "" && target != "local" {
		return false, nil
	}
	if r == nil || r.Root == "" {
		return false, errors.New("workspace is unavailable")
	}
	dir := filepath.Join(r.Root, "cron", "output")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	name := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return -1
	}, strings.ToLower(strings.TrimSpace(job.Name)))
	if name == "" {
		name = "job"
	}
	path := filepath.Join(dir, time.Now().UTC().Format("20060102-150405")+"-"+job.ID+"-"+name+".txt")
	if err := os.WriteFile(path, []byte(output), 0600); err != nil {
		return false, err
	}
	s.pruneOutputDir(dir)
	return true, nil
}

func (s *Store) pruneOutputDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type file struct {
		name string
		mod  time.Time
	}
	var files []file
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > 7*24*time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		files = append(files, file{e.Name(), info.ModTime()})
	}
	if len(files) <= 64 {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files[:len(files)-64] {
		_ = os.Remove(filepath.Join(dir, f.name))
	}
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
		// Restore once: a persisted running state with no live claim means
		// the owner is gone. Requeue that instant once, never replay N.
		if job.State == StateRunning && !live[job.ID] {
			job.State = StateScheduled
			if job.Pending.Instant > 0 {
				job.NextRunUnix = job.Pending.Instant
			}
			job.Pending = PendingSlot{}
			job.LastStatus = "restored_pending"
			changed = true
		}
		if job.NextRunUnix <= 0 || now.Unix() < job.NextRunUnix {
			continue
		}
		slot := job.NextRunUnix
		// Already fired never refires: a completed ledger row for this
		// exact instant suppresses the slot.
		if s.ledgerCompleted(job.ID, slot) {
			parsed, perr := ParseSchedule(job.Schedule, now)
			if perr != nil || !parsed.Repeat {
				job.NextRunUnix = 0
			} else {
				job.NextRunUnix = parsed.Next.Unix()
			}
			job.LastStatus = "skipped_duplicate_slot"
			changed = true
			continue
		}
		period := SchedulePeriod(job.Schedule, now)
		if period > 0 {
			half := period / 2
			if half < 120*time.Second {
				half = 120 * time.Second
			}
			if half > 2*time.Hour {
				half = 2 * time.Hour
			}
			catchUp := true
			if job.CatchUp != nil {
				catchUp = *job.CatchUp
			}
			if !catchUp && now.Sub(time.Unix(slot, 0)) > half {
				parsed, perr := ParseSchedule(job.Schedule, now)
				if perr != nil || !parsed.Repeat {
					job.NextRunUnix = 0
				} else {
					job.NextRunUnix = parsed.Next.Unix()
				}
				job.LastStatus = "skipped_catchup"
				changed = true
				continue
			}
		} else {
			// One-shots past a short grace retire with a diagnostic.
			if now.Sub(time.Unix(slot, 0)) > 2*time.Hour {
				job.NextRunUnix = 0
				job.LastStatus = "retired_missed_oneshot"
				changed = true
				continue
			}
		}
		parsed, err := ParseSchedule(job.Schedule, now)
		if err != nil {
			job.LastStatus = "blocked_config"
			job.LastError = "schedule no longer parses"
			changed = true
			continue
		}
		job.LastInstant = slot
		if !parsed.Repeat {
			job.NextRunUnix = 0
		} else {
			job.NextRunUnix = parsed.Next.Unix()
		}
		job.State = StateRunning
		job.Pending = PendingSlot{Instant: slot, ClaimedUnix: now.Unix()}
		job.UpdatedUnix = now.Unix()
		changed = true
		due = append(due, *job)
	}
	if changed {
		_ = s.saveJobs(jobs)
	}
	return due
}

func runScriptFile(ctx context.Context, r *Runner, script, workdir string, boundSecs int) (string, error) {
	abs := script
	if r != nil && r.Resolve != nil {
		resolved, err := r.Resolve(script)
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
	if workdir != "" {
		cmd.Dir = workdir
	}
	env := []string{"PATH=/usr/bin:/bin:/usr/local/bin", "LANG=C.UTF-8"}
	if r != nil {
		if r.Scratch != "" {
			env = append(env, "TMPDIR="+r.Scratch)
		}
		if r.Root != "" {
			env = append(env, "OSWALD_WORKSPACE="+r.Root)
		}
	}
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &boundedWriter{buf: &buf, limit: 64 << 10}
	cmd.Stderr = &boundedWriter{buf: &buf, limit: 64 << 10}
	err = cmd.Run()
	return strings.TrimRight(buf.String(), "\n"), err
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
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
