package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestOrderedMigrationsFreshAndIdempotent(t *testing.T) {
	db := openTestDB(t)
	registry := orderedMigrations()

	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(registry) {
		t.Fatalf("migration count = %d, want %d", count, len(registry))
	}
	for _, migration := range registry {
		var name, checksum, appliedAt string
		if err := db.SQL().QueryRow(`SELECT name, checksum, applied_at FROM schema_migration_versions WHERE version = ?`, migration.version).Scan(&name, &checksum, &appliedAt); err != nil {
			t.Fatalf("read migration %d: %v", migration.version, err)
		}
		if name != migration.name || checksum != migrationChecksum(migration) || len(checksum) != 64 || appliedAt == "" {
			t.Fatalf("migration %d = name %q checksum %q applied %q", migration.version, name, checksum, appliedAt)
		}
	}
	if err := db.runSchemaMigrations(context.Background(), registry); err != nil {
		t.Fatalf("repeat migrations: %v", err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil || count != len(registry) {
		t.Fatalf("idempotent migration count = %d, err %v", count, err)
	}

	for _, table := range []string{"privacy_operations", "privacy_invalidation_events", "derived_index_revisions", "derived_index_changes", "maintenance_runs", "websocket_device_authorizations", "websocket_clients", "websocket_bootstrap_state"} {
		assertSchemaObject(t, db, "table", table)
	}
	for _, column := range []string{"forgotten_at", "hard_delete_after", "lifecycle_request_id"} {
		assertTableColumn(t, db, "memory_entries", column)
	}
	for _, column := range []string{"claim_key", "claim_slot", "claim_value", "evidence_count"} {
		assertTableColumn(t, db, "memory_entries", column)
	}
	for _, column := range []string{"claim_key", "claim_slot", "claim_value"} {
		assertTableColumn(t, db, "memory_candidates", column)
	}
	for _, column := range []string{"provenance_type", "relation_type", "confidence_contribution", "extraction_model", "extractor_version", "source_session_generation", "correlation_key"} {
		assertTableColumn(t, db, "memory_evidence", column)
	}
	for _, index := range []string{"idx_memory_candidates_claim_key", "idx_memory_entries_claim_key", "idx_memory_entries_claim_slot_status"} {
		assertSchemaObject(t, db, "index", index)
	}
	assertTableColumn(t, db, "account_users", "lifecycle_state")
	assertTableColumn(t, db, "memory_events", "canonical_user_id")
	assertTableColumn(t, db, "memory_formation_audit", "content_expires_at")
	assertTableColumn(t, db, "memory_formation_audit", "redacted_at")
}

func TestOrderedMigrationsRejectChecksumDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	db, err := Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`UPDATE schema_migration_versions SET checksum = ? WHERE version = 7`, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(path, config.NewLogger(config.LevelError))
	if err == nil || !strings.Contains(err.Error(), "checksum drift") {
		t.Fatalf("expected checksum drift error, got %v", err)
	}
}

func TestWebSocketDeviceAuthorizationSchemaConstraints(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SQL().Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES ('ws-user', '2026-07-19T12:00:00Z', '2026-07-19T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO websocket_device_authorizations (device_code_hash, user_code_hash, requested_client_name, expires_at, created_at, updated_at) VALUES (zeroblob(31), zeroblob(32), 'client', '2026-07-19T12:10:00Z', '2026-07-19T12:00:00Z', '2026-07-19T12:00:00Z')`); err == nil {
		t.Fatal("accepted an invalid device-code hash")
	}
	if _, err := db.SQL().Exec(`INSERT INTO websocket_clients (client_id, canonical_user_id, websocket_identifier, client_name, refresh_token_hash, created_at) VALUES ('client-id-is-long-enough', 'ws-user', 'ws-id', 'client', zeroblob(32), '2026-07-19T12:00:00Z')`); err == nil {
		t.Fatal("accepted a refresh hash without an expiry")
	}
	if _, err := db.SQL().Exec(`INSERT INTO websocket_clients (client_id, canonical_user_id, websocket_identifier, client_name, created_at) VALUES ('client-id-is-long-enough', 'ws-user', 'ws-id', 'client', '2026-07-19T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM account_users WHERE canonical_user_id = 'ws-user'`); err != nil {
		t.Fatal(err)
	}
	var clients int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM websocket_clients`).Scan(&clients); err != nil || clients != 0 {
		t.Fatalf("client cascade count=%d err=%v", clients, err)
	}
}

func TestMigrationChecksumCoversEverySQLDefinition(t *testing.T) {
	db := openTestDB(t)
	registry := orderedMigrations()
	tests := []struct {
		name      string
		index     int
		operation string
	}{
		{name: "v7 account lifecycle column", index: 6, operation: phase7EnsureAccountLifecycleOperation},
		{name: "v7 memory entries rebuild", index: 6, operation: phase7MemoryEntriesRebuildSQL},
		{name: "v7 memory events rebuild", index: 6, operation: phase7MemoryEventsRebuildSQL},
		{name: "v7 audit expiry column", index: 6, operation: phase7EnsureAuditContentExpiry},
		{name: "v7 audit redaction column", index: 6, operation: phase7EnsureAuditRedactedAt},
		{name: "v7 foundation", index: 6, operation: phase7FoundationSQL},
		{name: "v8 privacy operations rebuild", index: 7, operation: phase8PrivacyOperationsRebuildSQL},
		{name: "v8 trigger corrections", index: 7, operation: phase8PrivacyTriggerCorrectionsSQL},
		{name: "v9 privacy operation retention", index: 8, operation: phase9PrivacyOperationRetentionTriggerSQL},
		{name: "v9 audit retention", index: 8, operation: phase9AuditRetentionTriggerSQL},
		{name: "v1 account tables", index: 0, operation: accountUsersBaselineSQL},
		{name: "v1 account column operations", index: 0, operation: baselineV1AccountColumnOperations},
		{name: "v1 linked accounts", index: 0, operation: linkedAccountsBaselineSQL},
		{name: "v1 account challenges", index: 0, operation: accountLinkChallengesBaselineSQL},
		{name: "v1 memory tables", index: 0, operation: userMemoryBaselineSQL},
		{name: "v1 memory column operations", index: 0, operation: baselineV1MemoryColumnOperations},
		{name: "v1 cleanup indexes", index: 0, operation: userMemoryCleanupIndexesSQL},
		{name: "v1 profile index", index: 0, operation: baselineV1ProfileIndexSQL},
		{name: "v1 MCP schema", index: 0, operation: mcpServersBaselineSQL},
		{name: "v2 category transformation", index: 1, operation: baselineV2CategoryTransform},
		{name: "v2 approval transformation", index: 1, operation: baselineV2ApprovalTransform},
		{name: "v3 column operations", index: 2, operation: baselineV3ColumnOperations},
		{name: "v3 schema", index: 2, operation: memoryFormationSchemaSQL},
		{name: "v3 backfill", index: 2, operation: memoryFormationBackfillSQL},
		{name: "v4 column operations", index: 3, operation: baselineV4ColumnOperations},
		{name: "v4 schema", index: 3, operation: sessionCompactionSchemaSQL},
		{name: "v5 optional schema", index: 4, operation: memoryFTSSchemaSQL},
		{name: "v6 optional schema", index: 5, operation: sessionTurnsFTSSchemaSQL},
		{name: "v10 event redacted column", index: 9, operation: phase10MemoryEventsRedactedAtOperation},
		{name: "v10 event updated column", index: 9, operation: phase10MemoryEventsUpdatedAtOperation},
		{name: "v10 event backfill", index: 9, operation: phase10MemoryEventsBackfillSQL},
		{name: "v10 event trigger", index: 9, operation: phase10MemoryEventsRedactionTriggerSQL},
		{name: "v11 privacy invalidation outbox", index: 10, operation: phase11PrivacyInvalidationOutboxSQL},
		{name: "v12 websocket device authorization", index: 11, operation: phase12WebSocketDeviceAuthorizationSQL},
		{name: "v13 confidence evidence memory", index: 12, operation: phase13ConfidenceEvidenceMemorySQL},
		{name: "v14 deployment memory", index: 13, operation: phase14DeploymentMemorySQL},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := append([]schemaMigration(nil), registry...)
			migration := registry[test.index]
			mutated[test.index].definition = strings.Replace(migration.definition, test.operation, test.operation+"\n-- mutated operation", 1)
			if mutated[test.index].definition == migration.definition {
				t.Fatal("operation is absent from the migration definition")
			}
			if migrationChecksum(mutated[test.index]) == migrationChecksum(migration) {
				t.Fatal("mutated SQL operation retained its checksum")
			}
			err := db.runSchemaMigrations(context.Background(), mutated)
			if err == nil || !strings.Contains(err.Error(), "checksum drift") {
				t.Fatalf("expected checksum drift error, got %v", err)
			}
		})
	}
}

func TestPhase13MigratesConfidenceEvidenceMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phase13.db")
	handle, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &DB{path: path, db: handle}
	registry := orderedMigrations()
	if err := fixture.runSchemaMigrations(context.Background(), registry[:12]); err != nil {
		t.Fatalf("create v12 fixture: %v", err)
	}
	if _, err := handle.Exec(`
INSERT INTO account_users (canonical_user_id, created_at, updated_at)
VALUES ('phase13-user', '2026-07-20T12:00:00Z', '2026-07-20T12:00:00Z');
INSERT INTO memory_entries (
	canonical_user_id, scope, category, statement, statement_key, evidence,
	confidence, importance, status, created_at, updated_at
) VALUES (
	'phase13-user', 'long_term', 'projects', 'The user is building Oswald.',
	'the user is building oswald.', 'The user said so.', 0.8, 3, 'active',
	'2026-07-20T12:00:00Z', '2026-07-20T12:00:00Z'
);
INSERT INTO memory_candidates (
	canonical_user_id, idempotency_key, state, scope, category, statement, statement_key,
	evidence_summary, confidence, importance, provenance_type, source_authority,
	formation_mode, sensitivity, policy_decision, supersedes_memory_id, created_at, updated_at,
	confirmation_session_id, confirmation_request_id, confirmation_presented_at
) VALUES
	('phase13-user', 'explicit', 'pending_confirmation', 'long_term', 'identity',
	 'The user has a preferred name.', 'the user has a preferred name.', 'Call me Oz.',
	 0.1, 3, 'user_statement', 'user_direct', 'explicit_remember', 'identity_or_contact',
	 'pending_confirmation', 1, '2026-07-20T12:01:00Z', '2026-07-20T12:01:00Z',
	 'session', 'request', '2026-07-20T12:02:00Z'),
	('phase13-user', 'confident', 'pending_confirmation', 'long_term', 'relationships',
	 'The user works with Pat.', 'the user works with pat.', 'I work with Pat.',
	 0.35, 3, 'user_statement', 'user_direct', 'automatic_extraction', 'high_impact_interaction',
	 'pending_confirmation', NULL, '2026-07-20T12:03:00Z', '2026-07-20T12:03:00Z',
	 'session', 'request', '2026-07-20T12:04:00Z'),
	('phase13-user', 'uncertain', 'pending_confirmation', 'long_term', 'notes',
	 'The user may enjoy hiking.', 'the user may enjoy hiking.', 'Maybe I enjoy hiking.',
	 0.34, 3, 'user_statement', 'user_direct', 'automatic_extraction', 'low',
	 'pending_confirmation', NULL, '2026-07-20T12:05:00Z', '2026-07-20T12:05:00Z',
	 'session', 'request', '2026-07-20T12:06:00Z'),
	('phase13-user', 'already-approved', 'approved', 'long_term', 'environment',
	 'The user works remotely.', 'the user works remotely.', 'I work remotely.',
	 0.9, 3, 'user_statement', 'user_direct', 'automatic_extraction', 'low',
	 'automatic', NULL, '2026-07-20T12:07:00Z', '2026-07-20T12:07:00Z',
	 'old-session', 'old-request', '2026-07-20T12:08:00Z');
INSERT INTO memory_evidence (
	canonical_user_id, candidate_id, idempotency_key, evidence_type, content,
	source_authority, created_at
) VALUES ('phase13-user', 1, 'evidence', 'exact_user_quote', 'Call me Oz.',
	'user_direct', '2026-07-20T12:01:00Z');
INSERT INTO memory_evidence (
	canonical_user_id, memory_id, idempotency_key, evidence_type, content,
	source_authority, created_at
) VALUES
	('phase13-user', 1, 'memory-evidence-1', 'exact_user_quote', 'I am building Oswald.', 'user_direct', '2026-07-20T12:00:00Z'),
	('phase13-user', 1, 'memory-evidence-2', 'exact_user_quote', 'My project is Oswald.', 'user_direct', '2026-07-20T12:00:01Z');
INSERT INTO memory_confirmation_presentations (
	canonical_user_id, candidate_id, session_id, session_generation, request_id, delivered_at
) VALUES ('phase13-user', 1, 'session', 1, 'request', '2026-07-20T12:02:00Z');
`); err != nil {
		t.Fatalf("seed v12 fixture: %v", err)
	}

	if err := fixture.runSchemaMigrations(context.Background(), registry); err != nil {
		t.Fatalf("apply v13: %v", err)
	}
	if err := fixture.runSchemaMigrations(context.Background(), registry); err != nil {
		t.Fatalf("repeat v13: %v", err)
	}

	var entryClaimKey, entryClaimSlot, entryClaimValue string
	var evidenceCount int
	if err := handle.QueryRow(`SELECT claim_key, claim_slot, claim_value, evidence_count FROM memory_entries WHERE id = 1`).Scan(&entryClaimKey, &entryClaimSlot, &entryClaimValue, &evidenceCount); err != nil {
		t.Fatal(err)
	}
	if entryClaimKey != "legacy:1" || entryClaimSlot != "projects.legacy" || entryClaimValue != "the user is building oswald." || evidenceCount != 2 {
		t.Fatalf("entry backfill = key %q slot %q value %q evidence_count %d", entryClaimKey, entryClaimSlot, entryClaimValue, evidenceCount)
	}

	type candidateState struct {
		state, decision, decidedBy, reason, claimKey, claimSlot, claimValue string
		decidedAt                                                           sql.NullString
	}
	for _, test := range []struct {
		key  string
		want candidateState
	}{
		{key: "explicit", want: candidateState{state: "approved", decision: "automatic", decidedBy: "explicit_user_request", reason: "user explicitly requested this memory", claimKey: "legacy:1", claimSlot: "identity.legacy", claimValue: "the user has a preferred name.", decidedAt: sql.NullString{String: "2026-07-20T12:01:00Z", Valid: true}}},
		{key: "confident", want: candidateState{state: "approved", decision: "automatic", decidedBy: "formation_policy", reason: "confidence threshold met", claimKey: "legacy:2", claimSlot: "relationships.legacy", claimValue: "the user works with pat.", decidedAt: sql.NullString{String: "2026-07-20T12:03:00Z", Valid: true}}},
		{key: "uncertain", want: candidateState{state: "proposed", decision: "proposed", reason: "candidate requires review", claimKey: "legacy:3", claimSlot: "notes.legacy", claimValue: "the user may enjoy hiking."}},
	} {
		var got candidateState
		var confirmationSession, confirmationRequest string
		var confirmationPresented sql.NullString
		if err := handle.QueryRow(`SELECT state, policy_decision, decided_by, decision_reason, claim_key, claim_slot, claim_value, decided_at, confirmation_session_id, confirmation_request_id, confirmation_presented_at FROM memory_candidates WHERE idempotency_key = ?`, test.key).Scan(&got.state, &got.decision, &got.decidedBy, &got.reason, &got.claimKey, &got.claimSlot, &got.claimValue, &got.decidedAt, &confirmationSession, &confirmationRequest, &confirmationPresented); err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("candidate %s = %#v, want %#v", test.key, got, test.want)
		}
		if confirmationSession != "" || confirmationRequest != "" || confirmationPresented.Valid {
			t.Fatalf("candidate %s retained confirmation metadata", test.key)
		}
	}
	var approvedConfirmationSession, approvedConfirmationRequest string
	var approvedConfirmationPresented sql.NullString
	if err := handle.QueryRow(`SELECT confirmation_session_id, confirmation_request_id, confirmation_presented_at FROM memory_candidates WHERE idempotency_key = 'already-approved'`).Scan(&approvedConfirmationSession, &approvedConfirmationRequest, &approvedConfirmationPresented); err != nil {
		t.Fatal(err)
	}
	if approvedConfirmationSession != "" || approvedConfirmationRequest != "" || approvedConfirmationPresented.Valid {
		t.Fatal("already-approved candidate retained confirmation metadata")
	}

	var provenance, relation, extractionModel, extractorVersion, correlationKey, sensitivity string
	var contribution float64
	var sourceGeneration, supersedesID, presentations int
	if err := handle.QueryRow(`SELECT provenance_type, relation_type, confidence_contribution, extraction_model, extractor_version, source_session_generation, correlation_key FROM memory_evidence WHERE idempotency_key = 'evidence'`).Scan(&provenance, &relation, &contribution, &extractionModel, &extractorVersion, &sourceGeneration, &correlationKey); err != nil {
		t.Fatal(err)
	}
	if provenance != "user_statement" || relation != "supports" || contribution != 0.1 || extractionModel != "" || extractorVersion != "" || sourceGeneration != 0 || correlationKey != "" {
		t.Fatalf("evidence defaults = provenance %q relation %q contribution %v model %q version %q generation %d correlation %q", provenance, relation, contribution, extractionModel, extractorVersion, sourceGeneration, correlationKey)
	}
	if err := handle.QueryRow(`SELECT sensitivity, supersedes_memory_id FROM memory_candidates WHERE idempotency_key = 'explicit'`).Scan(&sensitivity, &supersedesID); err != nil {
		t.Fatal(err)
	}
	if sensitivity != "identity_or_contact" || supersedesID != 1 {
		t.Fatalf("preserved candidate fields = sensitivity %q supersedes %d", sensitivity, supersedesID)
	}
	if err := handle.QueryRow(`SELECT COUNT(*) FROM memory_confirmation_presentations`).Scan(&presentations); err != nil || presentations != 0 {
		t.Fatalf("confirmation presentations = %d, err %v", presentations, err)
	}
	var foreignKeyViolations int
	if err := handle.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations); err != nil || foreignKeyViolations != 0 {
		t.Fatalf("foreign key violations = %d, err %v", foreignKeyViolations, err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalOrderedFixturesUpgradeThroughV11(t *testing.T) {
	for _, version := range []int{7, 8, 9, 10} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "historical.db")
			handle, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
			if err != nil {
				t.Fatal(err)
			}
			fixture := &DB{path: path, db: handle}
			if err := fixture.runSchemaMigrations(context.Background(), orderedMigrations()[:version]); err != nil {
				t.Fatalf("create v%d fixture: %v", version, err)
			}
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}

			upgraded, err := Open(path, config.NewLogger(config.LevelError))
			if err != nil {
				t.Fatalf("upgrade v%d fixture: %v", version, err)
			}
			defer upgraded.Close() // nolint:errcheck
			assertTableColumn(t, upgraded, "memory_events", "redacted_at")
			assertTableColumn(t, upgraded, "memory_events", "updated_at")
			assertSchemaObject(t, upgraded, "table", "privacy_invalidation_events")
			var count int
			if err := upgraded.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil || count != len(orderedMigrations()) {
				t.Fatalf("upgraded migration count=%d err=%v", count, err)
			}
		})
	}
}

func TestPriorSymbolicV9LedgerRebasesSafely(t *testing.T) {
	path := filepath.Join(t.TempDir(), "symbolic-v9.db")
	handle, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &DB{path: path, db: handle}
	if err := fixture.runSchemaMigrations(context.Background(), orderedMigrations()[:9]); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES ('modern-user', datetime('now'), datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Exec(`INSERT INTO memory_entries(canonical_user_id, scope, category, statement, statement_key, evidence, status, created_at, updated_at, profile_approved, provenance_type, source_authority, source_request_id, formation_mode, sensitivity, approval_state, approved_at, valid_from) VALUES ('modern-user', 'long_term', 'notes', 'inferred modern fact', 'inferred modern fact', 'model inference', 'active', datetime('now'), datetime('now'), 0, 'model_inference', 'model_inference', 'request-modern', 'post_turn_extraction', 'sensitive', 'proposed', '', datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	for version, checksum := range legacySymbolicChecksums {
		if _, err := handle.Exec(`UPDATE schema_migration_versions SET checksum = ? WHERE version = ?`, checksum, version); err != nil {
			t.Fatal(err)
		}
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("upgrade prior symbolic v9 ledger: %v", err)
	}
	defer upgraded.Close() // nolint:errcheck
	for _, migration := range orderedMigrations()[:6] {
		var checksum string
		if err := upgraded.SQL().QueryRow(`SELECT checksum FROM schema_migration_versions WHERE version = ?`, migration.version).Scan(&checksum); err != nil {
			t.Fatal(err)
		}
		if checksum != migrationChecksum(migration) {
			t.Fatalf("version %d checksum was not rebased", migration.version)
		}
	}
	var profileApproved int
	var provenance, sourceAuthority, requestID, formationMode, sensitivity, approvalState string
	if err := upgraded.SQL().QueryRow(`SELECT profile_approved, provenance_type, source_authority, source_request_id, formation_mode, sensitivity, approval_state FROM memory_entries WHERE canonical_user_id = 'modern-user'`).Scan(&profileApproved, &provenance, &sourceAuthority, &requestID, &formationMode, &sensitivity, &approvalState); err != nil {
		t.Fatal(err)
	}
	if profileApproved != 0 || provenance != "model_inference" || sourceAuthority != "model_inference" || requestID != "request-modern" || formationMode != "post_turn_extraction" || sensitivity != "sensitive" || approvalState != "proposed" {
		t.Fatalf("symbolic rebase changed modern memory: approved=%d provenance=%q authority=%q request=%q mode=%q sensitivity=%q state=%q", profileApproved, provenance, sourceAuthority, requestID, formationMode, sensitivity, approvalState)
	}
}

func TestChecksumDriftPreventsPendingSchemaMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drift-v9.db")
	handle, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &DB{path: path, db: handle}
	if err := fixture.runSchemaMigrations(context.Background(), orderedMigrations()[:9]); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Exec(`UPDATE schema_migration_versions SET checksum = ? WHERE version = 7`, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path, config.NewLogger(config.LevelError)); err == nil || !strings.Contains(err.Error(), "checksum drift") {
		t.Fatalf("expected checksum drift, got %v", err)
	}
	inspect, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer inspect.Close() // nolint:errcheck
	for _, column := range []string{"redacted_at", "updated_at"} {
		var count int
		if err := inspect.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('memory_events') WHERE name = ?`, column).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("pending v10 column %s was created before drift rejection", column)
		}
	}
}

func TestMigrationLedgerGapPreventsPendingSchemaMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap-v9.db")
	handle, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close() // nolint:errcheck
	fixture := &DB{path: path, db: handle}
	if err := fixture.runSchemaMigrations(context.Background(), orderedMigrations()[:9]); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Exec(`DELETE FROM schema_migration_versions WHERE version = 4`); err != nil {
		t.Fatal(err)
	}
	if err := fixture.runSchemaMigrations(context.Background(), orderedMigrations()); err == nil || !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("migration ledger gap error = %v", err)
	}
	var phase10Columns int
	if err := handle.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('memory_events') WHERE name = 'redacted_at'`).Scan(&phase10Columns); err != nil {
		t.Fatal(err)
	}
	if phase10Columns != 0 {
		t.Fatal("pending migration mutated schema after ledger gap")
	}
}

func TestMemoryEventRedactionStartsTombstoneRetentionClock(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id,created_at,updated_at) VALUES ('user','2026-07-18T12:00:00Z','2026-07-18T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO memory_events(canonical_user_id,event_type,request_id,session_id,created_at,metadata) VALUES ('user','forgotten','request','session','2020-01-01T00:00:00Z','sensitive')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`UPDATE memory_events SET request_id='', session_id='', metadata='' WHERE canonical_user_id='user'`); err != nil {
		t.Fatal(err)
	}
	var createdAt, redactedAt, updatedAt string
	if err := db.SQL().QueryRow(`SELECT created_at, redacted_at, updated_at FROM memory_events WHERE canonical_user_id='user'`).Scan(&createdAt, &redactedAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if createdAt != "2020-01-01T00:00:00Z" || redactedAt == "" || updatedAt != redactedAt {
		t.Fatalf("created=%q redacted=%q updated=%q", createdAt, redactedAt, updatedAt)
	}
}

func TestOrderedMigrationsSerializeConcurrentRunners(t *testing.T) {
	db := openPrePhase7DB(t)
	const runners = 6
	stores := make([]*DB, 0, runners)
	for range runners {
		handle, err := sql.Open("sqlite3", db.path+"?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, &DB{path: db.path, db: handle})
		t.Cleanup(func() { handle.Close() }) // nolint:errcheck
	}

	start := make(chan struct{})
	errs := make(chan error, runners)
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Add(1)
		go func(store *DB) {
			defer wg.Done()
			<-start
			errs <- store.runSchemaMigrations(context.Background(), orderedMigrations())
		}(store)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil || count != len(orderedMigrations()) {
		t.Fatalf("concurrent migration count = %d, err %v", count, err)
	}
}

func TestOpenSerializesConcurrentFreshDatabaseMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-open.db")
	const runners = 6
	start := make(chan struct{})
	results := make(chan error, runners)
	stores := make(chan *DB, runners)
	var wg sync.WaitGroup
	for range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store, err := Open(path, config.NewLogger(config.LevelError))
			if err == nil {
				stores <- store
			}
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(stores)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
	for store := range stores {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	verify, err := Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close() // nolint:errcheck
	var count int
	if err := verify.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil || count != len(orderedMigrations()) {
		t.Fatalf("migration count=%d err=%v", count, err)
	}
}

func TestOrderedMigrationRollsBackFailedApply(t *testing.T) {
	db := openTestDB(t)
	sentinel := errors.New("injected migration failure")
	failing := schemaMigration{
		version:    len(orderedMigrations()) + 1,
		name:       "rollback_test",
		definition: "test-only rollback migration",
		apply: func(ctx context.Context, conn *sql.Conn) error {
			if _, err := conn.ExecContext(ctx, `CREATE TABLE migration_rollback_probe (id INTEGER PRIMARY KEY)`); err != nil {
				return err
			}
			return sentinel
		},
	}
	registry := append(orderedMigrations(), failing)
	err := db.runSchemaMigrations(context.Background(), registry)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected injected failure, got %v", err)
	}
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'migration_rollback_probe'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed migration left its schema change behind")
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM schema_migration_versions WHERE version = ?`, failing.version).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed migration ledger count = %d, err %v", count, err)
	}
}

func TestPhase8CorrectsExistingPhase7Database(t *testing.T) {
	db := openPrePhase7DB(t)
	registry := orderedMigrations()
	if err := db.runSchemaMigrations(context.Background(), registry[:7]); err != nil {
		t.Fatalf("apply phase 7: %v", err)
	}

	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	hashC := strings.Repeat("c", 64)
	if _, err := db.SQL().Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES ('phase8-user', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO privacy_operations (
	operation_id, idempotency_key, actor_hash, target_user_id, target_hash,
	operation_type, target_digest, status, created_at, updated_at, completed_at
) VALUES ('old-operation', 'old-key', ?, 'phase8-user', ?, 'export_user', ?, 'completed',
	'2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, hashA, hashB, hashC); err != nil {
		t.Fatalf("insert phase 7 operation: %v", err)
	}
	if _, err := db.SQL().Exec(`
INSERT INTO memory_formation_audit (
	canonical_user_id, idempotency_key, event_type, request_id, actor_type, created_at
) VALUES ('phase8-user', 'audit-key', 'formed', 'sensitive-request', 'system', '2026-07-18T12:00:00Z')`); err != nil {
		t.Fatalf("insert audit record: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO privacy_operations (
	operation_id, idempotency_key, actor_hash, target_user_id, target_hash,
	operation_type, target_digest, status, created_at, updated_at, completed_at
) VALUES ('new-operation-before-v8', 'new-key-before-v8', ?, 'phase8-user', ?, 'delete_memory', ?, 'completed',
	'2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, hashA, hashB, hashC); err == nil {
		t.Fatal("phase 7 accepted a phase 8 operation type")
	}
	if _, err := db.SQL().Exec(`UPDATE memory_formation_audit SET request_id = '', redacted_at = '2026-07-18T12:01:00Z' WHERE idempotency_key = 'audit-key'`); err == nil {
		t.Fatal("phase 7 allowed privacy audit redaction")
	}
	if _, err := db.SQL().Exec(`UPDATE account_users SET lifecycle_state = 'erasing' WHERE canonical_user_id = 'phase8-user'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM privacy_operations WHERE operation_id = 'old-operation'`); err == nil {
		t.Fatal("phase 7 allowed erasing-user operation cleanup")
	}

	if err := db.runSchemaMigrations(context.Background(), registry); err != nil {
		t.Fatalf("upgrade phase 7 database to phase 8: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO privacy_operations (
	operation_id, idempotency_key, actor_hash, target_user_id, target_hash,
	operation_type, target_digest, status, created_at, updated_at, completed_at
) VALUES ('new-operation', 'new-key', ?, 'phase8-user', ?, 'delete_memory', ?, 'completed',
	'2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, hashA, hashB, hashC); err != nil {
		t.Fatalf("phase 8 operation type: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE memory_formation_audit SET request_id = '', redacted_at = '2026-07-18T12:01:00Z' WHERE idempotency_key = 'audit-key'`); err != nil {
		t.Fatalf("phase 8 audit redaction: %v", err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM privacy_operations WHERE target_user_id = 'phase8-user'`); err != nil {
		t.Fatalf("phase 8 erasing-user operation cleanup: %v", err)
	}
}

func TestPhase9AllowsOnlyDependencySafePrivacyTombstoneDeletion(t *testing.T) {
	db := openPrePhase7DB(t)
	registry := orderedMigrations()
	if err := db.runSchemaMigrations(context.Background(), registry[:8]); err != nil {
		t.Fatalf("apply through phase 8: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id,created_at,updated_at) VALUES ('active-user','2026-07-18T12:00:00Z','2026-07-18T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	insert := func(id, status string, target any) {
		t.Helper()
		var challengeHash any = ""
		var challengeExpiry any
		if status == "pending" {
			challengeHash = "challenge"
			challengeExpiry = "2026-07-18T13:00:00Z"
		}
		_, err := db.SQL().Exec(`INSERT INTO privacy_operations(operation_id,idempotency_key,actor_hash,target_user_id,target_hash,operation_type,target_digest,challenge_hash,challenge_expires_at,status,created_at,updated_at,completed_at) VALUES (?,?,?,?,?,'export_user',?,?,?,?,?,?,?)`, id, id, hash, target, hash, hash, challengeHash, challengeExpiry, status, "2026-07-18T12:00:00Z", "2026-07-18T12:00:00Z", "2026-07-18T12:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("target-null", "completed", nil)
	insert("active-target", "completed", "active-user")
	insert("pending", "pending", "active-user")
	if _, err := db.SQL().Exec(`DELETE FROM privacy_operations WHERE operation_id = 'target-null'`); err == nil {
		t.Fatal("phase 8 unexpectedly allowed target-null tombstone deletion")
	}
	if err := db.runSchemaMigrations(context.Background(), registry); err != nil {
		t.Fatalf("upgrade phase 8 database to phase 9: %v", err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM privacy_operations WHERE operation_id = 'target-null'`); err != nil {
		t.Fatalf("phase 9 target-null tombstone deletion: %v", err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM privacy_operations WHERE operation_id = 'active-target'`); err != nil {
		t.Fatalf("phase 9 completed active-target tombstone deletion: %v", err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM privacy_operations WHERE operation_id = 'pending'`); err == nil {
		t.Fatal("phase 9 allowed pending operation deletion")
	}
}

func TestPhase7MigratesLegacyMemoryEvents(t *testing.T) {
	db := openPrePhase7DB(t)
	if _, err := db.SQL().Exec(`
INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES
	('owner', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z');
INSERT INTO memory_entries (
	canonical_user_id, scope, category, statement, statement_key, evidence, created_at, updated_at
) VALUES (
	'owner', 'long_term', 'notes', 'Owns a telescope.', 'owns a telescope.', 'User said so.',
	'2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z'
);
INSERT INTO memory_events (memory_id, event_type, created_at) VALUES
	(1, 'created', '2026-07-18T12:00:00Z'),
	(NULL, 'legacy_orphan', '2026-07-18T12:00:01Z');
`); err != nil {
		t.Fatal(err)
	}
	if err := db.runSchemaMigrations(context.Background(), orderedMigrations()); err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}

	var userID, eventType string
	if err := db.SQL().QueryRow(`SELECT canonical_user_id, event_type FROM memory_events`).Scan(&userID, &eventType); err != nil {
		t.Fatal(err)
	}
	if userID != "owner" || eventType != "created" {
		t.Fatalf("migrated event = user %q type %q", userID, eventType)
	}
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM memory_events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("memory event count = %d, err %v", count, err)
	}
	if _, err := db.SQL().Exec(`UPDATE memory_entries SET status = 'forgotten', forgotten_at = '2026-07-18T12:01:00Z', hard_delete_after = '2026-07-25T12:01:00Z', lifecycle_request_id = 'request-1' WHERE id = 1`); err != nil {
		t.Fatalf("set forgotten lifecycle: %v", err)
	}
}

func TestPhase7SchemaConstraintsAndTenantOwnership(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SQL().Exec(`
INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES
	('user-a', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z'),
	('user-b', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z');
INSERT INTO memory_entries (
	canonical_user_id, scope, category, statement, statement_key, evidence, created_at, updated_at
) VALUES ('user-a', 'long_term', 'notes', 'Fact.', 'fact.', 'Evidence.', '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z');
`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO memory_events (canonical_user_id, memory_id, event_type, created_at) VALUES ('user-b', 1, 'read', '2026-07-18T12:00:00Z')`); err == nil {
		t.Fatal("cross-tenant memory event was accepted")
	}
	if _, err := db.SQL().Exec(`UPDATE account_users SET lifecycle_state = 'deleted' WHERE canonical_user_id = 'user-a'`); err == nil {
		t.Fatal("invalid account lifecycle was accepted")
	}

	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	hashC := strings.Repeat("c", 64)
	if _, err := db.SQL().Exec(`
INSERT INTO privacy_operations (
	operation_id, idempotency_key, actor_hash, target_user_id, target_hash,
	operation_type, target_digest, status, created_at, updated_at, completed_at
) VALUES (?, ?, ?, ?, ?, 'delete_user', ?, 'completed', ?, ?, ?)`,
		"operation-1", "key-1", hashA, "user-a", hashB, hashC,
		"2026-07-18T12:00:00Z", "2026-07-18T12:01:00Z", "2026-07-18T12:01:00Z",
	); err != nil {
		t.Fatalf("insert privacy operation: %v", err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM account_users WHERE canonical_user_id = 'user-a'`); err != nil {
		t.Fatalf("delete operation target: %v", err)
	}
	var target sql.NullString
	if err := db.SQL().QueryRow(`SELECT target_user_id FROM privacy_operations WHERE operation_id = 'operation-1'`).Scan(&target); err != nil || target.Valid {
		t.Fatalf("retained operation target = %v, err %v", target, err)
	}

	insertRevision := func(revision int) error {
		_, err := db.SQL().Exec(`
INSERT INTO derived_index_revisions (
	index_kind, schema_version, revision, table_name, state, expected_count,
	indexed_count, created_at, updated_at
) VALUES ('memory_fts', 1, ?, ?, 'live', 0, 0, '2026-07-18T12:00:00Z', '2026-07-18T12:00:00Z')`, revision, fmt.Sprintf("memory_fts_%d", revision))
		return err
	}
	if err := insertRevision(1); err != nil {
		t.Fatal(err)
	}
	if err := insertRevision(2); err == nil {
		t.Fatal("multiple live revisions for one index kind were accepted")
	}
}

func openPrePhase7DB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	handle, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	db := &DB{path: path, db: handle, log: config.NewLogger(config.LevelError)}
	t.Cleanup(func() { db.Close() }) // nolint:errcheck
	for _, initialize := range []func() error{
		db.initializeAccountUsers,
		db.initializeLinkedAccounts,
		db.initializeAccountLinkChallenges,
		db.initializeUserMemory,
	} {
		if err := initialize(); err != nil {
			t.Fatalf("initialize pre-phase7 database: %v", err)
		}
	}
	return db
}
