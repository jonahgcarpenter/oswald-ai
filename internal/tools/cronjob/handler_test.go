package cronjob

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

func testContext(requestID string) context.Context {
	ctx := context.Background()
	ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "alice", Assurance: identity.AssuranceDiscordGateway})
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: requestID})
	return ctx
}

func TestDefinitionContract(t *testing.T) {
	def := Definition()
	if def.Name != Name || def.Name == "" || def.Description == "" {
		t.Fatalf("invalid definition: %+v", def)
	}
	if def.Parameters.Type != "object" || def.Parameters.Properties == nil {
		t.Fatalf("invalid parameters: %+v", def)
	}
}

func TestCronjobManageCreateListRun(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "job.sh"), []byte("#!/bin/sh\necho cron-hi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron"), requestctx.Metadata{RequestID: "req-cron", SessionID: "discord:chan1:987654321"})
	manage := NewCronjobManageHandler(ws, store)
	created, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "script": "job.sh", "no_agent": true})
	if err != nil || !strings.Contains(created.Content, "job_id:") {
		t.Fatalf("create = %+v %v", created, err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
	stored, err := store.Resolve(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Deliver.Platform != "discord" || stored.Deliver.ChatID != "chan1" || stored.Deliver.UserID != "987654321" {
		t.Fatalf("deliver not defaulted to creating chat: %+v", stored.Deliver)
	}
	listed, err := manage(ctx, map[string]interface{}{"action": "list"})
	if err != nil || !strings.Contains(listed.Content, jobID) {
		t.Fatalf("list = %+v %v", listed, err)
	}
	run, err := manage(ctx, map[string]interface{}{"action": "run", "job_id": jobID})
	if err != nil || !strings.Contains(run.Content, "cron-hi") {
		t.Fatalf("run = %+v %v", run, err)
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "script": "job.sh", "model": "x"}); err == nil {
		t.Fatal("model override accepted")
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "prompt": "hi", "monitor_url": "https://x.example"}); err == nil {
		t.Fatal("dropped monitor field accepted")
	}
}

func TestCronjobManageZonedTimestamps(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron-tz"), requestctx.Metadata{RequestID: "req-cron-tz", SessionID: "discord:chan1:987654321"})
	manage := NewCronjobManageHandler(ws, store)
	// Far-future one-shot: asserts the zoned next_run_at rendering (the Nov
	// 2026 wall-clock regression) without depending on the test clock.
	created, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "2027-06-01T00:15:00Z", "prompt": "hi"})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	if !strings.Contains(created.Content, "next_run_at: 2027-06-01T00:15:00Z (") || !strings.Contains(created.Content, "UTC") {
		t.Fatalf("create missing zoned next_run_at: %q", created.Content)
	}
	listed, err := manage(ctx, map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed.Content, "next=2027-06-01T00:15:00Z (") || !strings.Contains(listed.Content, "UTC") {
		t.Fatalf("list missing zoned next: %q", listed.Content)
	}
}

func TestCronjobManageNameAndDeliver(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron-name"), requestctx.Metadata{RequestID: "req-cron-name", SessionID: "discord:chan1:987654321"})
	manage := NewCronjobManageHandler(ws, store)
	created, err := manage(ctx, map[string]interface{}{"action": "create", "name": "brief", "schedule": "in 1h", "prompt": "hi", "deliver_platform": "discord", "deliver_chat_id": "chan9", "deliver_user_id": "112233445"})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
	stored, err := store.Resolve(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "brief" || stored.Deliver.Platform != "discord" || stored.Deliver.ChatID != "chan9" || stored.Deliver.UserID != "112233445" {
		t.Fatalf("deliver not stored: %+v", stored)
	}
	// Fuzzy name resolution on update.
	updated, err := manage(ctx, map[string]interface{}{"action": "pause", "job_id": "brie", "paused_reason": "test"})
	if err != nil || !strings.Contains(updated.Content, "paused") {
		t.Fatalf("fuzzy pause = %+v %v", updated, err)
	}
	stored, err = store.Resolve(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != cron.StatePaused || stored.NextRunAt != nil || stored.PausedReason != "test" {
		t.Fatalf("pause = %+v", stored)
	}
	resumed, err := manage(ctx, map[string]interface{}{"action": "resume", "job_id": jobID})
	if err != nil || !strings.Contains(resumed.Content, "resumed") {
		t.Fatalf("resume = %+v %v", resumed, err)
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "create", "name": "brief-two", "schedule": "in 1h", "prompt": "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "remove", "job_id": "rief"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous remove = %v", err)
	}
}

func TestCronjobManageDeliverValidation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron-deliver"), requestctx.Metadata{RequestID: "req-cron-deliver", SessionID: "discord:chan1:987654321"})
	manage := NewCronjobManageHandler(ws, store)
	created, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "prompt": "hi"})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "deliver_platform": "sms"}); err == nil {
		t.Fatal("bad platform accepted")
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "deliver_chat_id": ""}); err == nil {
		t.Fatal("empty chat accepted")
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "deliver_user_id": "alice"}); err == nil {
		t.Fatal("display-name user accepted")
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "deliver_chat_id": "chan2"}); err != nil {
		t.Fatalf("chat update = %v", err)
	}
	stored, err := store.Resolve(jobID)
	if err != nil || stored.Deliver.ChatID != "chan2" || stored.Deliver.Platform != "discord" {
		t.Fatalf("deliver merge = %+v %v", stored, err)
	}
	// No session context and no explicit target fails closed.
	bare := testContext("req-cron-bare")
	if _, err := manage(bare, map[string]interface{}{"action": "create", "schedule": "in 1h", "prompt": "hi"}); err == nil {
		t.Fatal("missing deliver accepted without session")
	}
}

func TestCronjobManageMCPServers(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron-mcp"), requestctx.Metadata{RequestID: "req-cron-mcp", SessionID: "discord:chan1:987654321"})
	manage := NewCronjobManageHandler(ws, store)
	// Omitted exposes all.
	created, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "prompt": "hi"})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
	stored, err := store.Resolve(jobID)
	if err != nil || stored.MCPServers != nil {
		t.Fatalf("omitted = %+v %v", stored, err)
	}
	// Explicit list scopes.
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "mcp_servers": []interface{}{"homeassistant", "spotify"}}); err != nil {
		t.Fatalf("update = %v", err)
	}
	stored, _ = store.Resolve(jobID)
	if len(stored.MCPServers) != 2 || stored.MCPServers[0] != "homeassistant" {
		t.Fatalf("scoped = %+v", stored.MCPServers)
	}
	// Empty list disables.
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "mcp_servers": []interface{}{}}); err != nil {
		t.Fatalf("clear = %v", err)
	}
	stored, _ = store.Resolve(jobID)
	if stored.MCPServers == nil || len(stored.MCPServers) != 0 {
		t.Fatalf("cleared = %+v, want non-nil empty", stored.MCPServers)
	}
	// List shows the state.
	listed, err := manage(ctx, map[string]interface{}{"action": "list"})
	if err != nil || !strings.Contains(listed.Content, "mcp=none") {
		t.Fatalf("list = %+v %v", listed, err)
	}
	// Bad names and shapes fail closed.
	for _, args := range []map[string]interface{}{
		{"action": "create", "schedule": "in 1h", "prompt": "hi", "mcp_servers": []interface{}{"Spotify"}},
		{"action": "create", "schedule": "in 1h", "prompt": "hi", "mcp_servers": []interface{}{"*"}},
		{"action": "create", "schedule": "in 1h", "prompt": "hi", "mcp_servers": "homeassistant"},
	} {
		if _, err := manage(ctx, args); err == nil {
			t.Fatalf("bad allowlist accepted: %v", args["mcp_servers"])
		}
	}
}
