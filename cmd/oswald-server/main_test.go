package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/startup"
)

func TestNonInteractiveStartupOmitsBanner(t *testing.T) {
	if os.Getenv("OSWALD_STARTUP_TEST_HELPER") == "1" {
		if err := startup.Serve(context.Background(), config.OswaldHomeDir, os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}
	for _, tc := range []struct{ name, event, message string }{
		{"config loading", "app.config.invalid", "invalid runtime configuration"},
		{"startup validation", "app.profile_storage.init_failed", "failed to open profile state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNonInteractiveStartupOmitsBanner$")
			cmd.Dir = t.TempDir()
			if tc.name == "startup validation" {
				root := filepath.Join(cmd.Dir, ".oswald")
				if err := os.MkdirAll(filepath.Join(root, "profiles", "api"), 0700); err != nil {
					t.Fatal(err)
				}
				text := "providers:\n  fake:\n    api: http://127.0.0.1:9999/v1\nmodel:\n  provider: custom:fake\n  default: synthetic/model\nplatforms:\n  api:\n    enabled: true\n    extra:\n      api_port: 12345\n"
				if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite3", filepath.Join(root, "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`CREATE TABLE incompatible_fixture(value TEXT)`); err != nil {
					db.Close()
					t.Fatal(err)
				}
				db.Close()
			}
			// Both isolated cases fail before any storage or network work.
			cmd.Env = []string{"OSWALD_STARTUP_TEST_HELPER=1"}
			if testing.CoverMode() != "" {
				// Let the instrumented child record coverage without inheriting the parent environment.
				cmd.Env = append(cmd.Env, "GOCOVERDIR="+t.TempDir())
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err == nil {
				t.Fatal("expected configuration failure")
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected no banner on piped stdout: %q", stdout.String())
			}
			var event map[string]any
			failureCount := 0
			cleanupComplete := false
			for _, line := range bytes.Split(bytes.TrimSpace(stderr.Bytes()), []byte("\n")) {
				event = nil
				if err := json.Unmarshal(line, &event); err != nil {
					t.Fatalf("stderr contains a non-JSON event: %q: %v", line, err)
				}
				if event["event"] == "app.stopped" {
					cleanupComplete = true
				}
				if event["level"] == "error" {
					failureCount++
					if tc.name == "startup validation" && !cleanupComplete {
						t.Fatal("startup failure logged before cleanup completed")
					}
				}
			}
			if failureCount != 1 || event["event"] != tc.event || event["msg"] != tc.message || strings.Contains(stderr.String(), "\x1b") {
				t.Fatalf("unexpected startup log: %q", stderr.String())
			}
		})
	}
}
