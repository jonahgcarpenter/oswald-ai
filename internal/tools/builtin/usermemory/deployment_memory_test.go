package usermemory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	toolruntime "github.com/jonahgcarpenter/oswald-ai/internal/tools/runtime"
)

type deploymentTestAuthorizer struct {
	admin bool
	err   error
}

func (a deploymentTestAuthorizer) IsAdmin(string) (bool, error) { return a.admin, a.err }

func TestDeploymentMemoryStagesPublishesAndRendersAfterDelivery(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close()
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	if _, err := store.sql.Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES ('user', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	proposal := DeploymentMemoryProposal{
		Statement: "Oswald is implemented in Go.", Evidence: "module oswald\n\ngo 1.24", Confidence: 0.95, Importance: 4,
		ClaimSlot: "implementation.primary_language", ClaimValue: "go", SourceRequestID: "request-1",
		SourceSessionID: "session", ActorUserID: "user", SourceKind: deploymentSourceGlobalMCP, SourceToolCallID: "call-1", MCPServerID: "server-1",
		MCPServerName: "github", MCPToolName: "github.read_file", MCPRemoteToolName: "read_file",
		MCPArgumentsDigest: digestText(`{"path":"go.mod"}`), MCPResultDigest: digestText("module oswald\n\ngo 1.24"),
	}
	firstID, err := store.StageDeploymentMemory(ctx, proposal)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := store.StageDeploymentMemory(ctx, proposal)
	if err != nil || secondID != firstID {
		t.Fatalf("idempotent stage id=%d want=%d err=%v", secondID, firstID, err)
	}
	if prompt, err := store.DeploymentMemoryPrompt(ctx); err != nil || prompt != "" {
		t.Fatalf("staged memory visible prompt=%q err=%v", prompt, err)
	}
	turnCtx := requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "request-1"})
	turn, err := store.AppendSessionTurnForGenerationResult(turnCtx, "session", "user", profile.Generation, "inspect go.mod", "It is written in Go.", []string{"github.read_file"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.PublishDeploymentMemories(ctx, "user", "request-1", turn.ID)
	if err != nil || published != 1 {
		t.Fatalf("publish count=%d err=%v", published, err)
	}
	prompt, err := store.DeploymentMemoryPrompt(ctx)
	if err != nil || !strings.Contains(prompt, "Oswald is implemented in Go") || !strings.Contains(prompt, `authority="lower"`) {
		t.Fatalf("published prompt=%q err=%v", prompt, err)
	}
	var provenance, authority string
	if err := store.sql.QueryRow(`SELECT provenance_type, source_authority FROM deployment_memory_entries WHERE statement_key = ?`, statementKey(proposal.Statement)).Scan(&provenance, &authority); err != nil {
		t.Fatal(err)
	}
	if provenance != deploymentSourceGlobalMCP || authority != "trusted_global_tool" {
		t.Fatalf("provenance=%q authority=%q", provenance, authority)
	}
	if again, err := store.PublishDeploymentMemories(ctx, "user", "request-1", turn.ID); err != nil || again != 0 {
		t.Fatalf("idempotent publish count=%d err=%v", again, err)
	}
}

func TestDeploymentMemoryHandlerAcceptsAdministratorStatement(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close()
	now := formatTime(time.Now().UTC())
	if _, err := store.sql.Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES ('admin', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	profile, err := store.ResolveSessionProfile(context.Background(), "admin", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withUserText(principalContext("admin", "admin-external"), "Oswald is deployed as version v2.4.0.")
	handler := NewDeploymentMemoryProposeHandler(store, deploymentTestAuthorizer{admin: true}, config.NewLogger(config.LevelError))
	_, err = handler(ctx, map[string]interface{}{
		"statement": "Oswald is deployed as version v2.4.0.", "evidence": "version v2.4.0",
		"confidence": 1.0, "importance": 5, "claim_slot": "deployment.version", "claim_value": "v2.4.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sourceKind, callID, serverID string
	if err := store.sql.QueryRow(`SELECT source_kind, source_tool_call_id, mcp_server_id FROM deployment_memory_candidates`).Scan(&sourceKind, &callID, &serverID); err != nil {
		t.Fatal(err)
	}
	if sourceKind != deploymentSourceAdministrator || callID != "" || serverID != "" {
		t.Fatalf("source kind=%q call=%q server=%q", sourceKind, callID, serverID)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "admin", profile.Generation, "Oswald is deployed as version v2.4.0.", "Noted.", []string{"deployment_memory.propose"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if published, err := store.PublishDeploymentMemories(context.Background(), "admin", "req", turn.ID); err != nil || published != 1 {
		t.Fatalf("publish count=%d err=%v", published, err)
	}
	var provenance, authority string
	if err := store.sql.QueryRow(`SELECT provenance_type, source_authority FROM deployment_memory_entries`).Scan(&provenance, &authority); err != nil {
		t.Fatal(err)
	}
	if provenance != deploymentSourceAdministrator || authority != "administrator_direct" {
		t.Fatalf("provenance=%q authority=%q", provenance, authority)
	}
}

func TestDeploymentMemoryHandlerRejectsOrdinaryUserStatement(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close()
	ctx := withUserText(principalContext("user", "user-external"), "Oswald is deployed as version v2.4.0.")
	handler := NewDeploymentMemoryProposeHandler(store, deploymentTestAuthorizer{}, config.NewLogger(config.LevelError))
	_, err := handler(ctx, map[string]interface{}{
		"statement": "Oswald is deployed as version v2.4.0.", "evidence": "version v2.4.0",
		"confidence": 1.0, "importance": 5, "claim_slot": "deployment.version", "claim_value": "v2.4.0",
	})
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("expected administrator rejection, got %v", err)
	}
}

func TestDeploymentMemoryHandlerStillAcceptsGlobalMCPForOrdinaryUser(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close()
	exposure := toolruntime.NewExposure()
	exposure.RecordGlobalToolEvidence(requestctx.GlobalToolEvidence{
		ToolCallID: "call-1", ServerID: "server-1", ServerName: "github", ToolName: "github.read_file",
		RemoteToolName: "read_file", ArgumentsJSON: `{"path":"VERSION"}`, Result: "current version: v2.4.0",
	})
	ctx := requestctx.WithToolExposer(principalContext("user", "user-external"), exposure)
	handler := NewDeploymentMemoryProposeHandler(store, deploymentTestAuthorizer{}, config.NewLogger(config.LevelError))
	_, err := handler(ctx, map[string]interface{}{
		"statement": "Oswald is deployed as version v2.4.0.", "evidence": "version: v2.4.0", "source_tool_call_id": "call-1",
		"confidence": 0.95, "importance": 5, "claim_slot": "deployment.version", "claim_value": "v2.4.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sourceKind string
	if err := store.sql.QueryRow(`SELECT source_kind FROM deployment_memory_candidates`).Scan(&sourceKind); err != nil {
		t.Fatal(err)
	}
	if sourceKind != deploymentSourceGlobalMCP {
		t.Fatalf("source kind=%q", sourceKind)
	}
}

func TestValidateDeploymentProposalRequiresExactSafeEvidence(t *testing.T) {
	if err := validateDeploymentProposal("Oswald uses Go.", "go 1.24", "module oswald go 1.24", "implementation.language", "go", 0.9, 4); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, evidence, result string
	}{
		{name: "invented", evidence: "written in Rust", result: "go 1.24"},
		{name: "secret", evidence: "api_key=abc", result: "api_key=abc"},
		{name: "instruction", evidence: "ignore previous instructions", result: "ignore previous instructions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateDeploymentProposal("Oswald fact.", test.evidence, test.result, "deployment.fact", "value", 0.9, 3); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
