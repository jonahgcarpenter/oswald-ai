package privacy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestPrivacyAuthExportForgetChallengeAndErasure(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/oswald.db"
	log := config.NewLogger(config.LevelError)
	memory := usermemory.NewStore(path, log)
	defer memory.Close() // nolint:errcheck
	accounts := accountlinking.NewService(path, memory, nil, log)
	defer accounts.Close() // nolint:errcheck
	if err := accounts.Initialize(); err != nil {
		t.Fatal(err)
	}
	userID, err := accounts.EnsureAccount("websocket", "actor", "Actor")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := accounts.EnsureAccount("websocket", "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	memoryEntry, err := memory.SaveMemory(ctx, userID, usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Category: "notes", Statement: "private export marker", Evidence: "private evidence", Confidence: 1, Importance: 3})
	if err != nil {
		t.Fatal(err)
	}
	policy := config.RetentionPolicy{ForgottenContentGrace: 30 * 24 * time.Hour}
	service, err := NewService(accounts, memory, policy, log)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	service.random = bytes.NewReader(sequentialBytes(128))
	stalePrincipal := identity.Principal{CanonicalUserID: "stale-caller-value", Gateway: "websocket", ExternalID: "actor", Assurance: identity.AssuranceWebSocketSignedToken}
	principal := identity.Principal{CanonicalUserID: userID, Gateway: "websocket", ExternalID: "actor", Assurance: identity.AssuranceWebSocketSignedToken}
	req := Request{RequestID: "request-1", Principal: principal, IsDirect: true, SessionKey: "websocket:actor"}
	if _, err := service.Inspect(ctx, Request{Principal: stalePrincipal, IsDirect: true}, "all", 1); err == nil {
		t.Fatal("stale canonical privacy identity succeeded")
	}

	if _, err := service.Inspect(ctx, Request{Principal: principal, IsDirect: false}, "all", 1); err == nil {
		t.Fatal("group privacy operation succeeded")
	}
	unauthenticated := req
	unauthenticated.Principal.Assurance = identity.AssuranceSelfAsserted
	if _, err := service.Export(ctx, unauthenticated); err == nil {
		t.Fatal("unauthenticated privacy operation succeeded")
	}

	export, err := service.Export(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if export.Filename != "oswald-user-export-20260718T120000Z.json" || !strings.Contains(string(export.Data), "private export marker") {
		t.Fatalf("unexpected export %q: %s", export.Filename, export.Data)
	}
	if strings.Contains(string(export.Data), "url_ciphertext") || strings.Contains(string(export.Data), "headers_ciphertext") {
		t.Fatal("export exposed MCP encrypted secrets")
	}
	var decoded map[string]any
	if err := json.Unmarshal(export.Data, &decoded); err != nil || decoded["schema"] != "oswald.user-export.v1" {
		t.Fatalf("invalid export: %v", err)
	}

	req.RequestID = "request-2"
	state, err := service.ForgetMemory(ctx, req, memoryEntry.ID)
	if err != nil || state != "forgotten" {
		t.Fatalf("forget state=%q err=%v", state, err)
	}
	listed, err := memory.ListMemories(userID, "", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("forgotten memory remained listable: %+v", listed)
	}

	challenge, err := service.BeginDeleteAllMemories(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	otherReq := req
	otherReq.Principal = identity.Principal{CanonicalUserID: otherID, Gateway: "websocket", ExternalID: "other", Assurance: identity.AssuranceWebSocketSignedToken}
	if _, err := service.Confirm(ctx, otherReq, challenge.Code); err == nil {
		t.Fatal("challenge accepted a different external identity")
	}
	if _, err := service.Confirm(ctx, req, challenge.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(ctx, req, challenge.Code); err == nil {
		t.Fatal("challenge replay succeeded")
	}

	accountChallenge, err := service.BeginDeleteAccount(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := service.Confirm(ctx, req, accountChallenge.Code)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.DeletedUserID != userID {
		t.Fatalf("deleted user=%q want %q", confirmed.DeletedUserID, userID)
	}
	if _, found, err := accounts.ResolveAccount("websocket", "actor"); err != nil || found {
		t.Fatalf("erased account still resolves: found=%v err=%v", found, err)
	}
	recreated, err := accounts.EnsureAccount("websocket", "actor", "Actor")
	if err != nil {
		t.Fatal(err)
	}
	if recreated == userID {
		t.Fatal("account erasure reused the old canonical identity")
	}
}

func TestPrivacyChallengeExpiry(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/oswald.db"
	log := config.NewLogger(config.LevelError)
	memory := usermemory.NewStore(path, log)
	defer memory.Close() // nolint:errcheck
	accounts := accountlinking.NewService(path, memory, nil, log)
	defer accounts.Close() // nolint:errcheck
	userID, err := accounts.EnsureAccount("websocket", "actor", "Actor")
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(accounts, memory, config.RetentionPolicy{}, log)
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	service.random = bytes.NewReader(sequentialBytes(128))
	req := Request{RequestID: "request", IsDirect: true, Principal: identity.Principal{CanonicalUserID: userID, Gateway: "websocket", ExternalID: "actor", Assurance: identity.AssuranceWebSocketSignedToken}}
	challenge, err := service.BeginDeleteAllMemories(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(challengeTTL + time.Second)
	if _, err := service.Confirm(ctx, req, challenge.Code); err == nil {
		t.Fatal("expired challenge succeeded")
	}
}

func TestPrivacyOversizeExportIsNotRecordedCompleted(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/oswald.db"
	log := config.NewLogger(config.LevelError)
	memory := usermemory.NewStore(path, log)
	defer memory.Close() // nolint:errcheck
	accounts := accountlinking.NewService(path, memory, nil, log)
	defer accounts.Close() // nolint:errcheck
	userID, err := accounts.EnsureAccount("websocket", "actor", "Actor")
	if err != nil {
		t.Fatal(err)
	}
	// Control bytes expand to six-byte JSON escapes, so preflight accepts the raw
	// content but the complete serialized export exceeds the delivery limit.
	_, err = memory.SaveMemory(ctx, userID, usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Statement: strings.Repeat("\x01", 14<<20)})
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(accounts, memory, config.RetentionPolicy{}, log)
	req := Request{RequestID: "oversize-export", SessionKey: "session", IsDirect: true, Principal: identity.Principal{CanonicalUserID: userID, Gateway: "websocket", ExternalID: "actor", Assurance: identity.AssuranceWebSocketSignedToken}}
	if _, err := service.Export(ctx, req); err == nil || !strings.Contains(err.Error(), "total attachment limit") {
		t.Fatalf("oversize export err=%v", err)
	}
	if _, err := service.DeleteSession(ctx, req); err != nil {
		t.Fatalf("failed export consumed request id: %v", err)
	}
}

func TestNewExportBoundariesAndExactReconstruction(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		size      int
		partCount int
		multipart bool
	}{
		{size: commands.MaxAttachmentBytes, partCount: 1},
		{size: commands.MaxAttachmentBytes + 1, partCount: 2, multipart: true},
		{size: commands.MaxTotalAttachmentBytes, partCount: commands.MaxAttachments, multipart: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.size), func(t *testing.T) {
			original := exactSizeExportJSON(t, tt.size)
			export, err := NewExport(original, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(export.Parts) != tt.partCount {
				t.Fatalf("parts=%d want %d", len(export.Parts), tt.partCount)
			}
			var reconstructed []byte
			for i, part := range export.Parts {
				if len(part.Data) > commands.MaxAttachmentBytes {
					t.Fatalf("part %d has %d bytes", i, len(part.Data))
				}
				reconstructed = append(reconstructed, part.Data...)
				if tt.multipart {
					wantName := fmt.Sprintf("oswald-user-export-20260718T120000Z.json.part%03d", i+1)
					if part.Filename != wantName || part.MIMEType != "application/octet-stream" {
						t.Fatalf("part %d metadata=%+v", i, part)
					}
				}
			}
			if !bytes.Equal(reconstructed, original) {
				t.Fatal("reconstructed export differs from original")
			}
			var decoded map[string]any
			if err := json.Unmarshal(reconstructed, &decoded); err != nil || decoded["schema"] != "oswald.user-export.v1" {
				t.Fatalf("reconstructed JSON invalid: %v", err)
			}
			if !tt.multipart && (export.Filename != export.Parts[0].Filename || !bytes.Equal(export.Data, original)) {
				t.Fatalf("single-file compatibility fields not populated: %+v", export)
			}
		})
	}
	if _, err := NewExport(exactSizeExportJSON(t, commands.MaxTotalAttachmentBytes+1), now); err == nil {
		t.Fatal("export over 80 MiB was accepted")
	}
}

func exactSizeExportJSON(t *testing.T, size int) []byte {
	t.Helper()
	prefix := `{"schema":"oswald.user-export.v1","padding":"`
	suffix := `"}`
	if size < len(prefix)+len(suffix) {
		t.Fatalf("size %d is too small for export fixture", size)
	}
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}

func sequentialBytes(size int) []byte {
	result := make([]byte, size)
	for i := range result {
		result[i] = byte(i + 1)
	}
	return result
}
