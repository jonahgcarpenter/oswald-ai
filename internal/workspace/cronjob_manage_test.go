package workspace

import (
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
)

func oneshotJob(state string) *cron.Job {
	deliver := cron.Deliver{Platform: "imessage", ChatID: "chat1", UserID: "+1"}
	job := &cron.Job{
		ID:      "test-once",
		Name:    "oneshot",
		Prompt:  "say hi",
		NoAgent: false,
		Schedule: cron.Schedule{
			Kind:    cron.ScheduleOnce,
			RunAt:   "2026-11-03T00:15:00Z",
			Display: "once at 2026-11-03 00:15",
		},
		Deliver: deliver,
		Enabled: true,
		State:   state,
	}
	one := 1
	job.Repeat.Times = &one
	return job
}

// Regression: updating a one-shot schedule used to null NextRunAt, so the
// ticker's ClaimDue would skip the job and it would never fire.
func TestApplyCronUpdateOneshotKeepsNextRunAt(t *testing.T) {
	job := oneshotJob(cron.StateScheduled)
	// Compute the instant relative to now: ParseSchedule rejects past
	// one-shots, so a hardcoded date would rot into a failure.
	future := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second).Format(time.RFC3339)

	err := applyCronUpdate(nil, job, map[string]interface{}{
		"schedule": future,
	})
	if err != nil {
		t.Fatalf("applyCronUpdate: %v", err)
	}
	if job.NextRunAt == nil {
		t.Fatal("expected NextRunAt to be set for an updated one-shot, got nil (job would never fire)")
	}
	if got := *job.NextRunAt; got != future {
		t.Fatalf("expected NextRunAt %s, got %s", future, got)
	}
	if got := job.Schedule.RunAt; got != future {
		t.Fatalf("expected Schedule.RunAt %s, got %s", future, got)
	}
}

func TestApplyCronUpdateOneshotInPhrase(t *testing.T) {
	job := oneshotJob(cron.StateScheduled)
	before := time.Now().UTC()

	err := applyCronUpdate(nil, job, map[string]interface{}{
		"schedule": "in 30m",
	})
	if err != nil {
		t.Fatalf("applyCronUpdate: %v", err)
	}
	if job.NextRunAt == nil {
		t.Fatal("expected NextRunAt to be set for an in-phrase one-shot, got nil")
	}
	got, err := time.Parse(time.RFC3339, *job.NextRunAt)
	if err != nil {
		t.Fatalf("NextRunAt does not parse: %v", err)
	}
	// applyCronUpdate reads time.Now() internally, so allow slack around the
	// expected instant to avoid flakiness at second boundaries.
	diff := got.Sub(before.Add(30 * time.Minute))
	if diff < -time.Second || diff > 2*time.Second {
		t.Fatalf("expected NextRunAt ~%s, got %s (diff %s)", before.Add(30*time.Minute).Format(time.RFC3339), got.Format(time.RFC3339), diff)
	}
}

func TestApplyCronUpdateOneshotPausedStaysNull(t *testing.T) {
	job := oneshotJob(cron.StatePaused)
	job.NextRunAt = nil
	future := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second).Format(time.RFC3339)

	err := applyCronUpdate(nil, job, map[string]interface{}{
		"schedule": future,
	})
	if err != nil {
		t.Fatalf("applyCronUpdate: %v", err)
	}
	if job.NextRunAt != nil {
		t.Fatalf("expected NextRunAt to stay nil for a paused one-shot, got %s", *job.NextRunAt)
	}
}

func TestApplyCronUpdateRepeatsStillAdvance(t *testing.T) {
	deliver := cron.Deliver{Platform: "imessage", ChatID: "chat1", UserID: "+1"}
	job := &cron.Job{
		ID:       "test-repeat",
		Name:     "repeat",
		Prompt:   "say hi",
		Schedule: cron.Schedule{Kind: cron.ScheduleCron, Expr: "0 23 * * 1-5", Display: "0 23 * * 1-5"},
		Deliver:  deliver,
		Enabled:  true,
		State:    cron.StateScheduled,
	}

	err := applyCronUpdate(nil, job, map[string]interface{}{
		"schedule": "0 23 * * 1-5",
	})
	if err != nil {
		t.Fatalf("applyCronUpdate: %v", err)
	}
	if job.NextRunAt == nil {
		t.Fatal("expected NextRunAt to advance for a repeat schedule, got nil")
	}
	got, err := time.Parse(time.RFC3339, *job.NextRunAt)
	if err != nil {
		t.Fatalf("NextRunAt does not parse: %v", err)
	}
	if !got.After(time.Now().UTC()) {
		t.Fatalf("expected NextRun next after now, got %s", *job.NextRunAt)
	}
}
