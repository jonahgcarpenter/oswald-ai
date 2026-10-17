package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRunner(root string) *Runner {
	return &Runner{
		Resolve: func(raw string) (string, error) {
			abs := filepath.Join(root, raw)
			if _, err := os.Lstat(abs); err != nil {
				return "", err
			}
			return abs, nil
		},
		ResolveDir: func(raw string) (string, error) {
			abs := filepath.Join(root, raw)
			info, err := os.Stat(abs)
			if err != nil || !info.IsDir() {
				return "", err
			}
			return abs, nil
		},
		Root: root,
	}
}

func TestParseScheduleVectors(t *testing.T) {
	now := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	for _, raw := range []string{"30m", "every 2h", "every monday 9am", "0 9 * * *", "in 30m", time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)} {
		parsed, err := ParseSchedule(raw, now)
		if err != nil || !parsed.Next.After(now) {
			t.Fatalf("schedule %q = %+v %v", raw, parsed, err)
		}
	}
	if _, err := ParseSchedule("someday", now); err == nil {
		t.Fatal("invalid schedule accepted")
	}
}

func TestSchedulePeriod(t *testing.T) {
	now := time.Now()
	if got := SchedulePeriod("30m", now); got != 30*time.Minute {
		t.Fatalf("duration period = %v", got)
	}
	if got := SchedulePeriod("every 2h", now); got != 2*time.Hour {
		t.Fatalf("every period = %v", got)
	}
	if got := SchedulePeriod("in 30m", now); got != 0 {
		t.Fatalf("one-shot period = %v", got)
	}
	if got := SchedulePeriod("0 9 * * *", now); got != 24*time.Hour {
		t.Fatalf("daily cron period = %v", got)
	}
	if got := SchedulePeriod("*/15 * * * *", now); got != 15*time.Minute {
		t.Fatalf("quarter cron period = %v", got)
	}
}

func TestClaimDueAdvancesBeforeDispatch(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	script := filepath.Join(root, "job.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho tick-hi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	job, err := store.Create(now, testRunner(root), Job{Schedule: "in 1s", Script: "job.sh", NoAgent: true, Prompt: "", OriginGateway: "discord", OriginSession: "discord:chan1:user1"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now())
	if len(due) != 1 || due[0].ID != job.ID {
		t.Fatalf("due = %+v", due)
	}
	if due[0].NextRunUnix != 0 || due[0].State != StateRunning {
		t.Fatalf("not advanced: %+v", due[0])
	}
	d := &Dispatcher{Store: store, Runner: testRunner(root), Profile: "alice", Senders: map[string]Sender{"discord": stubSender{sent: make(map[string]string)}}}
	ex, out, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || ex.Status != "completed_script" || !strings.Contains(out, "tick-hi") {
		t.Fatalf("run = %+v %q %v", ex, out, err)
	}
	jobs, _ := store.List()
	for _, j := range jobs {
		if j.ID == job.ID && (j.NextRunUnix != 0 || j.State != StateScheduled) {
			t.Fatalf("one-shot not settled: %+v", j)
		}
	}
}

func TestClaimDueSkipsDuplicateSlot(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	job := mustCreate(t, store, Job{Schedule: "every 1h", Prompt: "hi"})
	slot := time.Now().Add(-time.Hour).Unix()
	store.appendLedger(Execution{JobID: job.ID, Instant: slot, StartedUnix: slot, FinishedUnix: slot, Status: "agent_completed", Delivered: true})
	_, err := store.mutateID(job.ID, func(j *Job) error {
		j.NextRunUnix = slot
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	due := store.ClaimDue(time.Now())
	if len(due) != 0 {
		t.Fatalf("duplicate slot refired: %+v", due)
	}
}

func TestClaimDueCatchUpSkip(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	skip := false
	job := mustCreate(t, store, Job{Schedule: "every 1h", Prompt: "hi", CatchUp: &skip})
	_, err := store.mutateID(job.ID, func(j *Job) error {
		j.NextRunUnix = time.Now().Add(-3 * time.Hour).Unix()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	due := store.ClaimDue(time.Now())
	if len(due) != 0 {
		t.Fatalf("catch-up disabled but fired: %+v", due)
	}
	updated, _ := store.Resolve(job.ID)
	if updated.LastStatus != "skipped_catchup" {
		t.Fatalf("status = %+v", updated)
	}
}

func TestClaimDueFiresDueOneShot(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	job := mustCreate(t, store, Job{Schedule: "in 1s", Prompt: "hi"})
	slot := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	_, err := store.mutateID(job.ID, func(j *Job) error {
		j.Schedule = slot.Format(time.RFC3339)
		j.NextRunUnix = slot.Unix()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	due := store.ClaimDue(time.Now())
	if len(due) != 1 || due[0].ID != job.ID {
		t.Fatalf("due = %+v", due)
	}
	if due[0].NextRunUnix != 0 || due[0].State != StateRunning {
		t.Fatalf("not advanced: %+v", due[0])
	}
	jobs, _ := store.List()
	for _, j := range jobs {
		if j.ID == job.ID && (j.NextRunUnix != 0 || j.LastStatus == "blocked_config") {
			t.Fatalf("one-shot not settled: %+v", j)
		}
	}
}

func TestRepeatCompletesJob(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	script := filepath.Join(root, "job.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	job := mustCreate(t, store, Job{Schedule: "in 1s", Script: "job.sh", NoAgent: true, Repeat: RepeatState{Times: 1}})
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now())
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	d := &Dispatcher{Store: store, Runner: testRunner(root), Profile: "alice"}
	if _, _, err := d.RunOne(context.Background(), due[0], time.Minute); err != nil {
		t.Fatal(err)
	}
	updated, _ := store.Resolve(job.ID)
	if updated.State != StateCompleted || updated.Repeat.Completed != 1 {
		t.Fatalf("repeat not completed: %+v", updated)
	}
	if len(store.ClaimDue(time.Now())) != 0 {
		t.Fatal("completed job refired")
	}
}

func TestResolveFuzzy(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	mustCreate(t, store, Job{Name: "daily-brief", Schedule: "in 1h", Prompt: "hi"})
	mustCreate(t, store, Job{Name: "daily-digest", Schedule: "in 1h", Prompt: "hi"})
	got, err := store.Resolve("daily-brief")
	if err != nil || got.Name != "daily-brief" {
		t.Fatalf("exact name = %+v %v", got, err)
	}
	got, err = store.Resolve("brief")
	if err != nil || got.Name != "daily-brief" {
		t.Fatalf("fuzzy = %+v %v", got, err)
	}
	if _, err := store.Resolve("daily"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous = %v", err)
	}
	if _, err := store.Resolve("nope"); err == nil {
		t.Fatal("unknown resolved")
	}
}

func TestLegacyRepeatBoolMigration(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cron")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	legacy := `[{"id":"abc","schedule":"in 1h","prompt":"hi","repeat":true,"paused":true,"next_run_at":99,"created_unix":1,"updated_unix":1}]`
	if err := os.WriteFile(filepath.Join(dir, "jobs.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	jobs, err := store.List()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list = %+v %v", jobs, err)
	}
	job := jobs[0]
	if job.State != StatePaused || !job.Paused {
		t.Fatalf("paused not derived: %+v", job)
	}
	catchUp := true
	if job.CatchUp == nil || *job.CatchUp != catchUp {
		t.Fatalf("catch-up not migrated: %+v", job.CatchUp)
	}
}

func TestNoAgentEmptySendsNothing(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	script := filepath.Join(root, "quiet.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrue\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ex, out, err := store.RunNow(context.Background(), testRunner(root), mustCreate(t, store, Job{Schedule: "in 1h", Script: "quiet.sh", NoAgent: true}).ID)
	if err != nil || out != "" || ex.Status != "completed_script" {
		t.Fatalf("quiet = %+v %q %v", ex, out, err)
	}
}

func TestDispatchAgentOriginDelivery(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	sent := make(map[string]string)
	d := &Dispatcher{
		Store: store, Runner: testRunner(root), Profile: "alice",
		Agent: func(ctx context.Context, call AgentCall) (AgentResult, error) {
			if !call.SkipMemory || !strings.HasPrefix(call.SessionKey, "cron:") || call.Profile != "alice" {
				t.Errorf("call = %+v", call)
			}
			return AgentResult{Text: "Bills 24 Rams 21 Final"}, nil
		},
		Senders: map[string]Sender{
			"discord": stubSender{sent: sent},
		},
		NewGeneration: func(ctx context.Context, profile, session string) error { return nil },
	}
	job := mustCreate(t, store, Job{
		Schedule: "in 1s", Prompt: "score?", Deliver: "origin",
		OriginGateway: "discord", OriginSession: "discord:chan1:user1",
	})
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now())
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	ex, out, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || !ex.Delivered || ex.Status == "delivery_failed" {
		t.Fatalf("dispatch = %+v %q %v", ex, out, err)
	}
	if got := sent["discord:chan1:user1"]; !strings.Contains(got, "Bills 24") || strings.Contains(got, "Scheduled job") {
		t.Fatalf("sent = %q", got)
	}
	_ = job
}

func TestDispatchSilentSuppresses(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	sent := make(map[string]string)
	d := &Dispatcher{
		Store: store, Runner: testRunner(root), Profile: "alice",
		Agent: func(ctx context.Context, call AgentCall) (AgentResult, error) {
			return AgentResult{Text: "[SILENT] wrote file"}, nil
		},
		Senders:       map[string]Sender{"discord": stubSender{sent: sent}},
		NewGeneration: func(ctx context.Context, profile, session string) error { return nil },
	}
	job := mustCreate(t, store, Job{
		Schedule: "in 1s", Prompt: "x", Deliver: "origin",
		OriginGateway: "discord", OriginSession: "discord:chan1:user1",
	})
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now())
	ex, _, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || !ex.Delivered || ex.Status != "completed_silent" {
		t.Fatalf("silent = %+v %v", ex, err)
	}
	if len(sent) != 0 {
		t.Fatalf("silent sent: %v", sent)
	}
	_ = job
}

func TestDispatchUnknownOriginFailsClosed(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	d := &Dispatcher{
		Store: store, Runner: testRunner(root), Profile: "alice",
		Agent: func(ctx context.Context, call AgentCall) (AgentResult, error) {
			return AgentResult{Text: "hi"}, nil
		},
		Senders:       map[string]Sender{},
		NewGeneration: func(ctx context.Context, profile, session string) error { return nil },
	}
	mustCreate(t, store, Job{Schedule: "in 1s", Prompt: "x", Deliver: "origin"})
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now())
	ex, _, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || ex.Delivered || ex.Status != "delivery_failed" {
		t.Fatalf("unknown origin = %+v %v", ex, err)
	}
}

func TestTickerWakeDispatchesDue(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	script := filepath.Join(root, "job.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho woken\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(time.Now(), testRunner(root), Job{Schedule: "in 1s", Script: "job.sh", NoAgent: true, OriginGateway: "discord", OriginSession: "discord:chan1:user1"}); err != nil {
		t.Fatal(err)
	}
	ticker := NewTicker(store, testRunner(root), 30*time.Second)
	ticker.SetDispatcher(&Dispatcher{Store: store, Runner: testRunner(root), Profile: "alice", Senders: map[string]Sender{"discord": stubSender{sent: make(map[string]string)}}})
	ticker.Start()
	defer ticker.Stop()
	time.Sleep(1200 * time.Millisecond)
	ticker.Wake()
	deadline := time.Now().Add(10 * time.Second)
	for {
		jobs, _ := store.List()
		done := false
		for _, job := range jobs {
			if job.LastStatus == "completed_script" {
				done = true
			}
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("wake did not dispatch the due job")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRunOneClaimContention(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	script := filepath.Join(root, "slow.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\necho done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	job := mustCreate(t, store, Job{Schedule: "in 1h", Script: "slow.sh", NoAgent: true})
	d := &Dispatcher{Store: store, Runner: testRunner(root), Profile: "alice"}
	if !store.TryClaim(job.ID) {
		t.Fatal("claim failed")
	}
	defer store.Release(job.ID)
	if _, _, err := d.RunOne(context.Background(), job, time.Minute); err == nil {
		t.Fatal("contended run allowed")
	}
}

type stubSender struct {
	sent map[string]string
	fail error
}

func (s stubSender) SendText(_ context.Context, _, sessionKey, text string) error {
	if s.fail != nil {
		return s.fail
	}
	s.sent[sessionKey] = text
	return nil
}

func mustCreate(t *testing.T, store *Store, in Job) Job {
	t.Helper()
	job, err := store.Create(time.Now(), testRunner(store.root), in)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestDeliverDefaultsToOriginAndRejectsOthers(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	job := mustCreate(t, store, Job{Schedule: "in 1h", Prompt: "hi"})
	if job.Deliver != "origin" {
		t.Fatalf("deliver default = %q", job.Deliver)
	}
	for _, target := range []string{"local", "all", "", "bot-chat", "discord:chan1", "  local  "} {
		if target == "" {
			continue
		}
		if _, err := store.Create(time.Now(), testRunner(root), Job{Schedule: "in 1h", Prompt: "hi", Deliver: target}); err == nil {
			t.Fatalf("target %q accepted", target)
		}
	}
	if _, err := store.Create(time.Now(), testRunner(root), Job{Schedule: "in 1h", Prompt: "hi", Deliver: "origin"}); err != nil {
		t.Fatalf("origin rejected: %v", err)
	}
}

func TestLegacyEmptyDeliverLoadsAsOrigin(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cron")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	legacy := `[{"id":"abc","schedule":"in 1h","prompt":"hi","next_run_at":99,"created_unix":1,"updated_unix":1}]`
	if err := os.WriteFile(filepath.Join(dir, "jobs.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	jobs, err := store.List()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list = %+v %v", jobs, err)
	}
	if jobs[0].Deliver != "origin" {
		t.Fatalf("legacy deliver = %q", jobs[0].Deliver)
	}
}

func TestLegacyLocalTargetFailsClosedAtDispatch(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	d := &Dispatcher{
		Store: store, Runner: testRunner(root), Profile: "alice",
		Agent: func(ctx context.Context, call AgentCall) (AgentResult, error) {
			return AgentResult{Text: "hi"}, nil
		},
		Senders:       map[string]Sender{"discord": stubSender{sent: make(map[string]string)}},
		NewGeneration: func(ctx context.Context, profile, session string) error { return nil },
	}
	job := mustCreate(t, store, Job{
		Schedule: "in 1s", Prompt: "x", Deliver: "origin",
		OriginGateway: "discord", OriginSession: "discord:chan1:user1",
	})
	if _, err := store.mutateID(job.ID, func(j *Job) error {
		j.Deliver = "local"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now())
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	ex, _, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || ex.Delivered || ex.Status != "delivery_failed" {
		t.Fatalf("legacy target = %+v %v", ex, err)
	}
	if _, err := os.Stat(filepath.Join(root, "cron", "output")); !os.IsNotExist(err) {
		t.Fatal("output directory was created")
	}
}
