package imagecache

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestWorkerImmediateSweepAndJoinedStop(t *testing.T) {
	root := t.TempDir()
	cache := New(root)
	path, err := cache.Save(context.Background(), "alice", samplePNG(t), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&logs)
	w := NewWorker(cache, log)
	w.Start()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		select {
		case <-deadline:
			t.Fatal("immediate sweep did not remove expired image")
		case <-time.After(10 * time.Millisecond):
		}
	}
	w.Stop()
	w.Stop()
	if !containsSweepStatus(logs.Bytes(), "ok") || !bytes.Contains(logs.Bytes(), []byte(`"removed_file_count":1`)) {
		t.Fatalf("missing successful immediate sweep measurement: %s", logs.String())
	}
}

func TestWorkerFailureAndCancellationMeasurements(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		setup      func(t *testing.T, root string) context.Context
	}{
		{"failure", "error", func(t *testing.T, root string) context.Context {
			if err := os.WriteFile(filepath.Join(root, "bad"), []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			t.Cleanup(cancel)
			return ctx
		}},
		{"cancellation", "ok", func(t *testing.T, _ string) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ctx := tc.setup(t, root)
			if tc.want == "error" {
				root = filepath.Join(root, "bad")
			}
			var logs bytes.Buffer
			log := config.NewLogger(config.LevelDebug)
			log.SetOutput(&logs)
			w := NewWorker(New(root), log)
			w.done = make(chan struct{})
			w.run(ctx, time.Hour)
			if !containsSweepStatus(logs.Bytes(), tc.want) {
				t.Fatalf("missing %s sweep measurement: %s", tc.want, logs.String())
			}
			if tc.name == "cancellation" && !bytes.Contains(logs.Bytes(), []byte(`"outcome":"canceled"`)) {
				t.Fatal("missing cancellation outcome")
			}
			if tc.want == "error" && !bytes.Contains(logs.Bytes(), []byte(`"event":"imagecache.sweep.failed"`)) {
				t.Fatal("missing WARN diagnostic")
			}
		})
	}
}

func containsSweepStatus(data []byte, status string) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		var record struct {
			Event   string         `json:"event"`
			Level   string         `json:"level"`
			Details map[string]any `json:"details"`
		}
		if json.Unmarshal(line, &record) != nil || record.Event != "imagecache.sweep.complete" || record.Level != "debug" {
			continue
		}
		got, exists := record.Details["status"]
		if status == "ok" {
			if !exists {
				return true
			}
			continue
		}
		if exists && got == status {
			return true
		}
	}
	return false
}
