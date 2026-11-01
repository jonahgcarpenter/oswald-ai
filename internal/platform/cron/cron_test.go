package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

func testRunner(root string) *Runner {
	return &Runner{
		Root:       root,
		ScriptsDir: filepath.Join(root, "scripts"),
		ResolveDir: func(raw string) (string, error) {
			abs := filepath.Join(root, raw)
			info, err := os.Stat(abs)
			if err != nil || !info.IsDir() {
				return "", err
			}
			return abs, nil
		},
	}
}

func writeScript(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
}

func testDeliver() Deliver {
	return Deliver{Platform: "discord", ChatID: "chan1", UserID: "user1"}
}

func mustParse(t *testing.T, raw string, now time.Time) (Schedule, time.Time) {
	t.Helper()
	sched, next, err := ParseSchedule(raw, now)
	if err != nil {
		t.Fatalf("schedule %q: %v", raw, err)
	}
	return sched, next
}

func TestParseScheduleVectors(t *testing.T) {
	now := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	for _, raw := range []string{"30m", "every 2h", "every monday 9am", "0 9 * * *", "in 30m", time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)} {
		sched, next, err := ParseSchedule(raw, now)
		if err != nil || !next.After(now) {
			t.Fatalf("schedule %q = %+v %v %v", raw, sched, next, err)
		}
		if next.Location() != time.UTC {
			t.Fatalf("schedule %q next not UTC: %v", raw, next)
		}
	}
	if _, _, err := ParseSchedule("someday", now); err == nil {
		t.Fatal("invalid schedule accepted")
	}
}

func TestParseScheduleKinds(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	sched, next, err := ParseSchedule("30m", now)
	if err != nil || sched.Kind != ScheduleInterval || sched.Minutes != 30 || sched.Display != "every 30m" {
		t.Fatalf("duration = %+v %v %v", sched, next, err)
	}
	if !next.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("duration next = %v", next)
	}
	sched, _, err = ParseSchedule("every monday 9am", now)
	if err != nil || sched.Kind != ScheduleCron || sched.Display != "every monday 9am" {
		t.Fatalf("every = %+v %v", sched, err)
	}
	if sched.Expr == "" {
		t.Fatal("every did not materialize a cron expr")
	}
	sched, next, err = ParseSchedule("in 30m", now)
	if err != nil || sched.Kind != ScheduleOnce || sched.RunAt != utcISO(next) {
		t.Fatalf("in-phrase = %+v %v %v", sched, next, err)
	}
	iso := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	sched, _, err = ParseSchedule(iso, now)
	if err != nil || sched.Kind != ScheduleOnce {
		t.Fatalf("rfc3339 = %+v %v", sched, err)
	}
	if _, _, err := ParseSchedule(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC).Format(time.RFC3339), now); err == nil {
		t.Fatal("past one-shot accepted")
	}
}

func TestScheduleNextVectors(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	next, err := ScheduleNext(Schedule{Kind: ScheduleInterval, Minutes: 30}, now)
	if err != nil || !next.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("interval next = %v %v", next, err)
	}
	next, err = ScheduleNext(Schedule{Kind: ScheduleCron, Expr: "0 9 * * *"}, now)
	if err != nil || next.Hour() != 9 || next.Minute() != 0 || !next.After(now) {
		t.Fatalf("cron next = %v %v", next, err)
	}
	runAt := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	next, err = ScheduleNext(Schedule{Kind: ScheduleOnce, RunAt: utcISO(runAt)}, now)
	if err != nil || !next.Equal(runAt) {
		t.Fatalf("once next = %v %v", next, err)
	}
	if _, err := ScheduleNext(Schedule{Kind: "weekly"}, now); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestSchedulePeriod(t *testing.T) {
	now := time.Date(2026, 6, 15, 8, 0, 0, 0, time.UTC)
	if got := SchedulePeriod(Schedule{Kind: ScheduleInterval, Minutes: 30}, now); got != 30*time.Minute {
		t.Fatalf("duration period = %v", got)
	}
	if got := SchedulePeriod(Schedule{Kind: ScheduleOnce, RunAt: utcISO(now.Add(time.Hour))}, now); got != 0 {
		t.Fatalf("one-shot period = %v", got)
	}
	if got := SchedulePeriod(Schedule{Kind: ScheduleCron, Expr: "0 9 * * *"}, now); got != 24*time.Hour {
		t.Fatalf("daily cron period = %v", got)
	}
	if got := SchedulePeriod(Schedule{Kind: ScheduleCron, Expr: "*/15 * * * *"}, now); got != 15*time.Minute {
		t.Fatalf("quarter cron period = %v", got)
	}
}

func TestCreateDefaults(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	sched, next := mustParse(t, "every 1h", now)
	job, err := store.Create(now, testRunner(root), Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver()})
	if err != nil {
		t.Fatal(err)
	}
	if job.Repeat.Times != nil {
		t.Fatalf("recurring repeat not forever: %+v", job.Repeat)
	}
	if job.NextRunAt == nil || *job.NextRunAt != utcISO(next) {
		t.Fatalf("next_run_at = %+v want %s", job.NextRunAt, utcISO(next))
	}
	if !job.Enabled || job.State != StateScheduled {
		t.Fatalf("state = %+v", job)
	}
	if job.CreatedAt != utcISO(now) || job.Deliver.Platform != "discord" {
		t.Fatalf("job = %+v", job)
	}
	// One-shots default to a single run.
	sched, _ = mustParse(t, "in 1h", now)
	once, err := store.Create(now, testRunner(root), Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver()})
	if err != nil {
		t.Fatal(err)
	}
	if once.Repeat.Times == nil || *once.Repeat.Times != 1 {
		t.Fatalf("one-shot repeat = %+v", once.Repeat)
	}
}

func TestCreatePaused(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	sched, _ := mustParse(t, "every 1h", now)
	job, err := store.Create(now, testRunner(root), Job{
		Schedule: sched, Prompt: "hi", Deliver: testDeliver(),
		State: StatePaused, PausedReason: "waiting",
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.Enabled || job.State != StatePaused || job.NextRunAt != nil {
		t.Fatalf("paused = %+v", job)
	}
	if job.PausedAt == nil || *job.PausedAt != utcISO(now) || job.PausedReason != "waiting" {
		t.Fatalf("paused fields = %+v", job)
	}
}

func TestCreateValidation(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	sched, _ := mustParse(t, "every 1h", now)
	cases := []struct {
		name string
		job  Job
	}{
		{"prompt required", Job{Schedule: sched, Deliver: testDeliver()}},
		{"no_agent requires script", Job{Schedule: sched, NoAgent: true, Deliver: testDeliver()}},
		{"bad platform", Job{Schedule: sched, Prompt: "hi", Deliver: Deliver{Platform: "sms", ChatID: "1"}}},
		{"missing chat", Job{Schedule: sched, Prompt: "hi", Deliver: Deliver{Platform: "discord"}}},
		{"unknown toolset", Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver(), EnabledToolsets: []string{"web"}}},
		{"missing script", Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver(), Script: "nope.sh"}},
		{"absolute script", Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver(), Script: "/etc/passwd"}},
		{"traversal script", Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver(), Script: "../evil.sh"}},
		{"injection", Job{Schedule: sched, Prompt: "jailbreak the model", Deliver: testDeliver()}},
	}
	for _, tc := range cases {
		if _, err := store.Create(now, testRunner(root), tc.job); err == nil {
			t.Fatalf("%s accepted", tc.name)
		}
	}
	writeScript(t, root, "ok.sh", "#!/bin/sh\necho hi\n")
	if _, err := store.Create(now, testRunner(root), Job{Schedule: sched, Script: "ok.sh", NoAgent: true, Deliver: testDeliver()}); err != nil {
		t.Fatalf("no_agent without prompt rejected: %v", err)
	}
	if _, err := store.Create(now, testRunner(root), Job{Schedule: sched, Prompt: "hi", Deliver: Deliver{Platform: "bluebubbles", ChatID: "chat1", UserID: "u1"}}); err != nil {
		t.Fatalf("bluebubbles alias rejected: %v", err)
	} else {
		jobs, _ := store.List()
		if jobs[len(jobs)-1].Deliver.Platform != "imessage" {
			t.Fatalf("alias not normalized: %+v", jobs[len(jobs)-1].Deliver)
		}
	}
}

func TestClaimDueAdvancesBeforeDispatch(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	writeScript(t, root, "job.sh", "#!/bin/sh\necho tick-hi\n")
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1s", now)
	job, err := store.Create(now, testRunner(root), Job{Schedule: sched, Script: "job.sh", NoAgent: true, Deliver: testDeliver()})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now().UTC())
	if len(due) != 1 || due[0].ID != job.ID {
		t.Fatalf("due = %+v", due)
	}
	if due[0].NextRunAt != nil || due[0].State != StateRunning {
		t.Fatalf("not advanced: %+v", due[0])
	}
	if due[0].SlotUnix == 0 {
		t.Fatal("claim did not stamp the slot")
	}
	d := &Dispatcher{Store: store, Runner: testRunner(root), Profile: "alice", Senders: map[string]Sender{"discord": stubSender{sent: make(map[string]string)}}}
	ex, out, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || ex.Status != "completed_script" || !strings.Contains(out, "tick-hi") {
		t.Fatalf("run = %+v %q %v", ex, out, err)
	}
	jobs, _ := store.List()
	for _, j := range jobs {
		if j.ID == job.ID && (j.NextRunAt != nil || j.State != StateCompleted) {
			t.Fatalf("one-shot not settled: %+v", j)
		}
	}
}

func TestClaimDueSkipsDuplicateSlot(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	now := time.Now().UTC()
	sched, _ := mustParse(t, "every 1h", now)
	job := mustCreate(t, store, Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver()})
	slot := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	store.appendLedger(Execution{JobID: job.ID, Instant: slot.Unix(), StartedUnix: slot.Unix(), FinishedUnix: slot.Unix(), Status: "agent_completed", Delivered: true})
	at := utcISO(slot)
	_, err := store.mutateID(job.ID, func(j *Job) error {
		j.NextRunAt = &at
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	due := store.ClaimDue(time.Now().UTC())
	if len(due) != 0 {
		t.Fatalf("duplicate slot refired: %+v", due)
	}
}

func TestClaimDueRetiresMissedOneShot(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1s", now)
	job := mustCreate(t, store, Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver()})
	slot := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	runAt := utcISO(slot)
	_, err := store.mutateID(job.ID, func(j *Job) error {
		j.Schedule = Schedule{Kind: ScheduleOnce, RunAt: runAt, Display: "once"}
		j.NextRunAt = &runAt
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if due := store.ClaimDue(time.Now().UTC()); len(due) != 0 {
		t.Fatalf("missed one-shot fired: %+v", due)
	}
	updated, _ := store.Resolve(job.ID)
	if updated.LastStatus != "retired_missed_oneshot" || updated.NextRunAt != nil {
		t.Fatalf("retire = %+v", updated)
	}
}

func TestClaimDueRestoresOrphanedRun(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	now := time.Now().UTC()
	sched, _ := mustParse(t, "every 1h", now)
	job := mustCreate(t, store, Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver()})
	_, err := store.mutateID(job.ID, func(j *Job) error {
		j.State = StateRunning
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// No live claim: the next scan restores without firing a phantom slot.
	_ = store.ClaimDue(time.Now().UTC())
	updated, _ := store.Resolve(job.ID)
	if updated.LastStatus != "restored_pending" && updated.State == StateRunning {
		t.Fatalf("orphan not restored: %+v", updated)
	}
}

func TestClaimDueBlockedConfig(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	now := time.Now().UTC()
	sched, _ := mustParse(t, "every 1h", now)
	job := mustCreate(t, store, Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver()})
	at := utcISO(now.Add(-time.Minute))
	_, err := store.mutateID(job.ID, func(j *Job) error {
		j.Schedule = Schedule{Kind: "weekly"}
		j.NextRunAt = &at
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if due := store.ClaimDue(time.Now().UTC()); len(due) != 0 {
		t.Fatalf("corrupt schedule fired: %+v", due)
	}
	updated, _ := store.Resolve(job.ID)
	if updated.LastStatus != "blocked_config" {
		t.Fatalf("status = %+v", updated)
	}
}

func TestRepeatCompletesJob(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	writeScript(t, root, "job.sh", "#!/bin/sh\necho hi\n")
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1s", now)
	one := 1
	job := mustCreate(t, store, Job{Schedule: sched, Script: "job.sh", NoAgent: true, Deliver: testDeliver(), Repeat: RepeatState{Times: &one}})
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now().UTC())
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
	if len(store.ClaimDue(time.Now().UTC())) != 0 {
		t.Fatal("completed job refired")
	}
}

func TestFailureStreakAutoPauses(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	now := time.Now().UTC()
	sched, _ := mustParse(t, "every 1h", now)
	job := mustCreate(t, store, Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver()})
	for i := 0; i < FailureStreakLimit; i++ {
		if store.finishRun(job.ID, "error", "boom", true) != (i == FailureStreakLimit-1) {
			t.Fatalf("auto-pause signal wrong at failure %d", i+1)
		}
	}
	updated, _ := store.Resolve(job.ID)
	if updated.State != StatePaused || updated.FailureStreak != FailureStreakLimit || updated.NextRunAt != nil {
		t.Fatalf("auto-pause = %+v", updated)
	}
	if updated.PausedReason == "" || updated.PausedAt == nil {
		t.Fatalf("auto-pause fields = %+v", updated)
	}
	// A success resets the streak.
	sched2, _ := mustParse(t, "every 1h", now)
	job2 := mustCreate(t, store, Job{Schedule: sched2, Prompt: "hi", Deliver: testDeliver()})
	store.finishRun(job2.ID, "error", "boom", true)
	store.finishRun(job2.ID, "agent_completed", "", false)
	updated2, _ := store.Resolve(job2.ID)
	if updated2.FailureStreak != 0 || updated2.State != StateScheduled {
		t.Fatalf("streak not reset: %+v", updated2)
	}
}

func TestResolveFuzzy(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1h", now)
	mustCreate(t, store, Job{Name: "daily-brief", Schedule: sched, Prompt: "hi", Deliver: testDeliver()})
	mustCreate(t, store, Job{Name: "daily-digest", Schedule: sched, Prompt: "hi", Deliver: testDeliver()})
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

func TestLegacyShapeFailsClosed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cron")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{
		`[{"id":"abc","schedule":"in 1h","prompt":"hi","next_run_at":99,"created_unix":1,"updated_unix":1}]`,
		`[{"id":"abc","schedule":{"kind":"cron"},"prompt":"hi","bogus_field":1,"deliver":{"platform":"discord","chat_id":"1"},"created_at":"2026-11-01T00:00:00Z"}]`,
		`[{"schedule":{"kind":"interval","minutes":30},"prompt":"hi","deliver":{"platform":"discord","chat_id":"1"},"created_at":"2026-11-01T00:00:00Z"}]`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "jobs.json"), []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		store := NewStore(root)
		if _, err := store.List(); err == nil || !strings.Contains(err.Error(), "manual update") {
			t.Fatalf("legacy accepted: %s", payload)
		}
	}
}

func TestNoAgentEmptySendsNothing(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	writeScript(t, root, "quiet.sh", "#!/bin/sh\ntrue\n")
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1h", now)
	ex, out, err := store.RunNow(context.Background(), testRunner(root), mustCreate(t, store, Job{Schedule: sched, Script: "quiet.sh", NoAgent: true, Deliver: testDeliver()}).ID)
	if err != nil || out != "" || ex.Status != "completed_script" {
		t.Fatalf("quiet = %+v %q %v", ex, out, err)
	}
}

func TestDispatchAgentDelivery(t *testing.T) {
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
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1s", now)
	mustCreate(t, store, Job{Schedule: sched, Prompt: "score?", Deliver: testDeliver()})
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now().UTC())
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	ex, out, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || !ex.Delivered || ex.Status == "delivery_failed" {
		t.Fatalf("dispatch = %+v %q %v", ex, out, err)
	}
	got := sent["discord:chan1:user1"]
	if !strings.Contains(got, "Bills 24") || !strings.HasPrefix(got, "<@user1> ") || strings.Contains(got, "Scheduled job") {
		t.Fatalf("sent = %q", got)
	}
}

func TestDispatchAgentDeliversAttachments(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	sent := make(map[string]string)
	sentFiles := make(map[string][]media.OutputAttachment)
	d := &Dispatcher{
		Store: store, Runner: testRunner(root), Profile: "alice",
		Agent: func(ctx context.Context, call AgentCall) (AgentResult, error) {
			return AgentResult{
				Text: "weekly sales",
				Attachments: []media.OutputAttachment{
					{Filename: "sales.txt", MIMEType: "text/plain", Data: []byte("row1")},
				},
			}, nil
		},
		Senders: map[string]Sender{
			"discord": stubSender{sent: sent, sentFiles: sentFiles},
		},
		NewGeneration: func(ctx context.Context, profile, session string) error { return nil },
	}
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1s", now)
	mustCreate(t, store, Job{Schedule: sched, Prompt: "sales?", Deliver: testDeliver()})
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now().UTC())
	if len(due) != 1 {
		t.Fatalf("due = %+v", due)
	}
	ex, _, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || !ex.Delivered || ex.Status == "delivery_failed" {
		t.Fatalf("dispatch = %+v %v", ex, err)
	}
	if got := sent["discord:chan1:user1"]; !strings.Contains(got, "weekly sales") {
		t.Fatalf("sent = %q", got)
	}
	files := sentFiles["discord:chan1:user1"]
	if len(files) != 1 || files[0].Filename != "sales.txt" || string(files[0].Data) != "row1" {
		t.Fatalf("attachments = %+v", files)
	}
}

func TestDispatchDMSkipsMention(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	sent := make(map[string]string)
	d := &Dispatcher{
		Store: store, Runner: testRunner(root), Profile: "alice",
		Agent: func(ctx context.Context, call AgentCall) (AgentResult, error) {
			return AgentResult{Text: "hello dm"}, nil
		},
		Senders:       map[string]Sender{"discord": stubSender{sent: sent}},
		NewGeneration: func(ctx context.Context, profile, session string) error { return nil },
	}
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1s", now)
	mustCreate(t, store, Job{Schedule: sched, Prompt: "x", Deliver: Deliver{Platform: "discord", ChatID: "user1", UserID: "user1"}})
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now().UTC())
	ex, _, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || !ex.Delivered {
		t.Fatalf("dm = %+v %v", ex, err)
	}
	if got := sent["discord:dm:user1"]; got != "hello dm" {
		t.Fatalf("dm sent = %q", got)
	}
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
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1s", now)
	mustCreate(t, store, Job{Schedule: sched, Prompt: "x", Deliver: testDeliver()})
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now().UTC())
	ex, _, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || !ex.Delivered || ex.Status != "completed_silent" {
		t.Fatalf("silent = %+v %v", ex, err)
	}
	if len(sent) != 0 {
		t.Fatalf("silent sent: %v", sent)
	}
}

func TestDispatchUnknownPlatformFailsClosed(t *testing.T) {
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
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1s", now)
	mustCreate(t, store, Job{Schedule: sched, Prompt: "x", Deliver: testDeliver()})
	time.Sleep(1100 * time.Millisecond)
	due := store.ClaimDue(time.Now().UTC())
	ex, _, err := d.RunOne(context.Background(), due[0], time.Minute)
	if err != nil || ex.Delivered || ex.Status != "delivery_failed" {
		t.Fatalf("unknown platform = %+v %v", ex, err)
	}
}

func TestTickerWakeDispatchesDue(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	writeScript(t, root, "job.sh", "#!/bin/sh\necho woken\n")
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1s", now)
	if _, err := store.Create(now, testRunner(root), Job{Schedule: sched, Script: "job.sh", NoAgent: true, Deliver: testDeliver()}); err != nil {
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
	writeScript(t, root, "slow.sh", "#!/bin/sh\nsleep 5\necho done\n")
	now := time.Now().UTC()
	sched, _ := mustParse(t, "in 1h", now)
	job := mustCreate(t, store, Job{Schedule: sched, Script: "slow.sh", NoAgent: true, Deliver: testDeliver()})
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
	sent      map[string]string
	sentFiles map[string][]media.OutputAttachment
	fail      error
}

func (s stubSender) Deliver(_ context.Context, _, sessionKey, text string, attachments []media.OutputAttachment) error {
	if s.fail != nil {
		return s.fail
	}
	s.sent[sessionKey] = text
	if s.sentFiles != nil && len(attachments) > 0 {
		s.sentFiles[sessionKey] = append(s.sentFiles[sessionKey], attachments...)
	}
	return nil
}

func mustCreate(t *testing.T, store *Store, in Job) Job {
	t.Helper()
	if in.Deliver.Platform == "" {
		in.Deliver = testDeliver()
	}
	job, err := store.Create(time.Now().UTC(), testRunner(store.root), in)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestConcurrentCreatesNoLoss(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	now := time.Now().UTC()
	sched, _ := mustParse(t, "every 1h", now)
	const n = 20
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := store.Create(now, testRunner(root), Job{Schedule: sched, Prompt: "hi", Deliver: testDeliver()})
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := store.List()
	if err != nil || len(jobs) != n {
		t.Fatalf("jobs = %d %v", len(jobs), err)
	}
}

func TestEveryParsesToUTCWallClock(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	// Schedules are UTC wall-clock by contract: the model converts local
	// intent before scheduling, so DST transitions never shift the fire.
	now := time.Date(2026, 10, 31, 9, 0, 5, 0, loc)
	_, next, err := ParseSchedule("every day 9am", now)
	if err != nil {
		t.Fatal(err)
	}
	if next.Location() != time.UTC || next.Hour() != 9 {
		t.Fatalf("daily UTC = %v", next)
	}
	if want := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("daily = %v, want %v", next, want)
	}
}

func TestNextCronDayOrSemanticsAndLeapDay(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	firstNext, err := ScheduleNext(Schedule{Kind: ScheduleCron, Expr: "0 9 1 * 1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC); !firstNext.Equal(want) {
		t.Fatalf("cron OR first = %v, want %v", firstNext, want)
	}
	secondNext, err := ScheduleNext(Schedule{Kind: ScheduleCron, Expr: "0 9 1 * 1"}, firstNext)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC); !secondNext.Equal(want) {
		t.Fatalf("cron OR second = %v, want %v", secondNext, want)
	}
	leap, err := ScheduleNext(Schedule{Kind: ScheduleCron, Expr: "0 0 29 2 *"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC); !leap.Equal(want) {
		t.Fatalf("leap day = %v, want %v", leap, want)
	}
	monday, err := ScheduleNext(Schedule{Kind: ScheduleCron, Expr: "0 9 * * 1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if monday.Weekday() != time.Monday {
		t.Fatalf("weekly cron = %v", monday)
	}
}
