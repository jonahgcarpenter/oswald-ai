package cron

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// RepairProfile must be permission-fix-only: a profile that never used cron
// keeps no cron/ or scripts/ directory after boot repair.
func TestRepairProfileCreatesNothing(t *testing.T) {
	root := t.TempDir()
	RepairProfile(root, "alice")
	for _, name := range []string{"cron", "scripts"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("%s exists after repair (err=%v), want absent", name, err)
		}
	}
}

// The 30s ClaimDue tick must not materialize a cron directory for jobless
// profiles: the lock degrades to in-process and the missing jobs file reads
// as empty.
func TestClaimDueCreatesNothing(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	if due := store.ClaimDue(time.Now()); len(due) != 0 {
		t.Fatalf("due = %d jobs, want none", len(due))
	}
	if _, err := os.Lstat(filepath.Join(root, "cron")); !os.IsNotExist(err) {
		t.Fatalf("cron exists after tick (err=%v), want absent", err)
	}
}

// First use still self-creates: saving a job materializes cron/jobs.json.
func TestFirstSaveCreatesCronDir(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	job, err := store.Create(time.Now(), nil, Job{
		Name:     "hello",
		Prompt:   "synthetic prompt",
		Schedule: Schedule{Kind: ScheduleOnce, RunAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		Deliver:  Deliver{Platform: "discord", ChatID: "chan1", UserID: "123456789"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == "" {
		t.Fatal("saved job has no id")
	}
	if _, err := os.Stat(filepath.Join(root, "cron", "jobs.json")); err != nil {
		t.Fatalf("jobs.json missing after first save: %v", err)
	}
}
