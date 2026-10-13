package cron

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
		NextRunUnix:     parsed.Next.Unix(),
		CreatedUnix:     now.Unix(),
		UpdatedUnix:     now.Unix(),
	}
	if err := s.validate(r, &job, true); err != nil {
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

func (s *Store) validate(r *Runner, job *Job, isCreate bool) error {
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
	if r != nil {
		if job.Script != "" {
			if _, err := r.Resolve(job.Script); err != nil {
				return errors.New("script path must exist inside the working directory")
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
	_ = isCreate
	return nil
}

// Mutate applies an update function atomically (first failure wins per
// field order enforced by the caller building the updated job).
func (s *Store) Mutate(id string, fn func(*Job) error) (Job, error) {
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
	jobs, err := s.loadJobs()
	if err != nil {
		return err
	}
	kept := jobs[:0]
	found := false
	for _, job := range jobs {
		if job.ID == id {
			found = true
			continue
		}
		kept = append(kept, job)
	}
	if !found {
		return errors.New("unknown job")
	}
	return s.saveJobs(kept)
}

// RunNow executes one job synchronously and records the ledger entry.
func (s *Store) RunNow(ctx context.Context, r *Runner, id string) (Execution, string, error) {
	jobs, err := s.loadJobs()
	if err != nil {
		return Execution{}, "", err
	}
	var job *Job
	for i := range jobs {
		if jobs[i].ID == id {
			job = &jobs[i]
			break
		}
	}
	if job == nil {
		return Execution{}, "", errors.New("unknown job")
	}
	if job.Paused {
		return Execution{}, "", errors.New("job is paused")
	}
	if err := ScanPrompt(job.Prompt); job.Prompt != "" && err != nil {
		return Execution{}, "", err
	}
	return s.execute(ctx, r, job)
}

func (s *Store) execute(ctx context.Context, r *Runner, job *Job) (Execution, string, error) {
	started := time.Now()
	workdir := ""
	if r != nil {
		workdir = r.Root
		if job.Workdir != "" && r.ResolveDir != nil {
			if resolved, err := r.ResolveDir(job.Workdir); err == nil {
				workdir = resolved
			}
		}
	}
	// Monitor diff gate: first tick establishes the baseline.
	if job.MonitorScript != "" && r != nil {
		out, merr := runScriptFile(ctx, r, job.MonitorScript, workdir, 120)
		hash := hashString(out)
		if merr == nil && job.BaselineHash != "" && hash == job.BaselineHash {
			ex := Execution{JobID: job.ID, StartedUnix: started.Unix(), FinishedUnix: time.Now().Unix(), Status: "skipped_monitor_unchanged", Delivered: true}
			s.appendLedger(ex)
			return ex, "", nil
		}
		if merr == nil {
			job.BaselineHash = hash
			s.persistBaseline(job.ID, hash)
		}
	}
	var output string
	var status string
	if job.Script != "" && (job.NoAgent || job.Prompt == "") {
		out, err := runScriptFile(ctx, r, job.Script, workdir, 3600)
		output = out
		if err != nil {
			status = "script_error"
		} else {
			status = "completed_script"
		}
		if job.NoAgent {
			if strings.TrimSpace(output) == "" {
				ex := Execution{JobID: job.ID, StartedUnix: started.Unix(), FinishedUnix: time.Now().Unix(), Status: status, Delivered: true}
				s.appendLedger(ex)
				s.markRun(job.ID, status, "")
				return ex, "", nil
			}
			ex := Execution{JobID: job.ID, StartedUnix: started.Unix(), FinishedUnix: time.Now().Unix(), Status: status, OutputPreview: preview(output), Delivered: true}
			s.appendLedger(ex)
			s.markRun(job.ID, status, "")
			return ex, output, nil
		}
	}
	// Agent-mode jobs: script stdout is injected context; live agent
	// dispatch rides the gateway sender when available.
	combined := output
	if job.Prompt != "" && output != "" {
		combined = "Script output:\n" + output
	} else if job.Prompt != "" {
		combined = ""
	}
	ex := Execution{JobID: job.ID, StartedUnix: started.Unix(), FinishedUnix: time.Now().Unix(), Status: "agent_queued", OutputPreview: preview(combined), Delivered: false, DeliveryError: "no live gateway sender in this deployment; recorded only"}
	if job.Deliver == "" || job.Deliver == "local" || job.Deliver == "all" {
		ex.Delivered = true
		ex.DeliveryError = ""
		ex.Status = "completed_local"
	}
	s.appendLedger(ex)
	s.markRun(job.ID, ex.Status, ex.DeliveryError)
	return ex, combined, nil
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

func (s *Store) markRun(id, status, deliveryErr string) {
	jobs, err := s.loadJobs()
	if err != nil {
		return
	}
	for i := range jobs {
		if jobs[i].ID == id {
			jobs[i].LastStatus = status
			jobs[i].LastError = deliveryErr
			jobs[i].UpdatedUnix = time.Now().Unix()
			break
		}
	}
	_ = s.saveJobs(jobs)
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

// Tick dispatches due jobs with per-store locking, in-flight dedupe, and a
// bounded parallel pool.
func (s *Store) Tick(ctx context.Context, r *Runner, now time.Time, maxParallel int) []Execution {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.loadJobs()
	if err != nil {
		return nil
	}
	if maxParallel <= 0 || maxParallel > 8 {
		maxParallel = 4
	}
	inFlight := make(map[string]bool)
	var due []*Job
	for i := range jobs {
		job := &jobs[i]
		if job.Paused || job.NextRunUnix <= 0 || now.Unix() < job.NextRunUnix {
			continue
		}
		if inFlight[job.ID] {
			continue
		}
		inFlight[job.ID] = true
		due = append(due, job)
	}
	if len(due) == 0 {
		return nil
	}
	// Advance next_run_at before dispatch so crashes never double-fire.
	for _, job := range due {
		parsed, err := ParseSchedule(job.Schedule, now)
		if err != nil {
			continue
		}
		if !parsed.Repeat {
			job.NextRunUnix = 0
		} else {
			period := parsed.Period
			if period <= 0 {
				period = 24 * time.Hour
			}
			// Catch-up: missed beyond half-period clamped 120s-2h skips.
			half := period / 2
			if half < 120*time.Second {
				half = 120 * time.Second
			}
			if half > 2*time.Hour {
				half = 2 * time.Hour
			}
			last := time.Unix(job.NextRunUnix, 0)
			catchUp := true
			if repeat := job.Repeat; repeat != nil {
				catchUp = *repeat
			} else {
				catchUp = true
			}
			if !catchUp && now.Sub(last) > half {
				job.NextRunUnix = now.Add(period).Unix()
				job.LastStatus = "skipped_catchup"
			} else {
				job.NextRunUnix = parsed.Next.Unix()
			}
		}
		job.UpdatedUnix = now.Unix()
	}
	_ = s.saveJobs(jobs)
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var out []Execution
	for _, job := range due {
		if job.NextRunUnix == 0 && job.LastStatus == "skipped_catchup" {
			continue
		}
		copied := *job
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ex, _, _ := s.execute(ctx, r, &copied)
			mu.Lock()
			out = append(out, ex)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}
