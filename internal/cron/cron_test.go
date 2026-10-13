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

func TestTickAdvancesBeforeDispatch(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	script := filepath.Join(root, "job.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho tick-hi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	job, err := store.Create(now, testRunner(root), Job{Schedule: "in 1s", Script: "job.sh", NoAgent: true, Prompt: ""})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	execs := store.Tick(context.Background(), testRunner(root), time.Now(), 2)
	if len(execs) != 1 || execs[0].Status != "completed_script" {
		t.Fatalf("tick = %+v", execs)
	}
	jobs, _ := store.List()
	for _, j := range jobs {
		if j.ID == job.ID && j.NextRunUnix != 0 {
			t.Fatalf("one-shot not cleared: %+v", j)
		}
	}
	if !strings.Contains(execs[0].OutputPreview, "tick-hi") {
		t.Fatalf("output = %q", execs[0].OutputPreview)
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

func mustCreate(t *testing.T, store *Store, in Job) Job {
	t.Helper()
	job, err := store.Create(time.Now(), testRunner(store.root), in)
	if err != nil {
		t.Fatal(err)
	}
	return job
}
