# v4 Release Validation

This runbook maps the current [v4 epic (#74)](https://github.com/jonahgcarpenter/oswald-ai/issues/74) and [release-validation issue (#80)](https://github.com/jonahgcarpenter/oswald-ai/issues/80) to checked-in tests and operator checks. Run every command from the repository root with no live LLM gateway, embedding route, MCP server, Discord, BlueBubbles, SearXNG, or other external service.

## Automated Evidence

| Criterion | Exact automated evidence |
| --- | --- |
| Ordinary and explicit wording form bounded private memory; multiple facts survive long prompts | `TestV4Issue80ReleaseEvaluation/formation_multiple_facts_and_remember_semantics`, `TestServicePublishesIndependentFactsFromLongTurn` |
| Corrections, weaker contradictions, reinforcement, inference upgrades, confidence, and provenance follow policy | `TestV4Issue80ReleaseEvaluation/conflict_authority_and_inference_upgrade`, `TestProposeCandidateBlocksWeakConflictAndSupersedesWithStrongerEvidence`, `TestDirectEvidenceUpgradesInferenceAndInferenceCannotReplaceDirect` |
| Canonical publication, evidence, supersession, events, profile advancement, and index enqueue are transactional and lease-fenced | `TestProposeCandidateAtomicallyPublishesApprovedPolicy`, `TestProposeCandidateRollsBackEveryPublicationStage`, `TestFormationLeaseFenceRollsBackCandidateAndCanonicalWrites` |
| Tenant memory, profile, hybrid retrieval, degraded retrieval, and MCP visibility remain isolated | `TestV4Issue80ReleaseEvaluation/tenant_isolation_and_hybrid_degradation`, `TestOfflineHybridRecallEvaluationCorpus`, `TestProcessNeverIncludesAnotherUsersTenantProfile`, `TestProviderDiscoveryToolsAreScopedToVisibleEnabledServers` |
| Soul and memory authority boundaries hold; no soul or user-memory mutation tool is exposed | `TestProcessUsesFreshOperatorManagedSoulAsSystemPrompt`, `TestProcessOffersRetrievalOnlyMemoryTools`, `TestRegisterDoesNotExposeSoulTools`, `TestRegisterExposesRetrievalOnlyUserMemoryTools` |
| Current global memory is admin-curated, lower-authority, read-only to the model, and independent of account erasure | `TestV4Issue80ReleaseEvaluation/admin_global_lifecycle_and_account_independence`, `TestCommandsRequireAdmin`, `TestRegisterCatalogOmitsRemovedGlobalMemoryTools`, `TestGlobalMemorySurvivesAccountDeletion` |
| Only successfully delivered turns affect formation, compaction, replay, transcript search, or recall | `TestV4Issue80ReleaseEvaluation/failed_delivery_has_no_serving_effect`, `TestExecuteEnqueuesFormationOnlyAfterResponseDelivery`, `TestExecuteEnqueuesCompactionOnlyAfterSuccessfulDelivery`, `TestFormationJobsRequireExactSuccessfullyDeliveredSource` |
| Injection, credential, authorization, and capability content cannot publish as memory | `TestV4Issue80ReleaseEvaluation/injection_credentials_and_authorization_are_rejected`, `TestLoggerSecretPromptAndMCPCanariesAreRedacted` |
| Summary-plus-tail continuity and exact current-session transcript search remain bounded and scoped | `TestV4Issue80ReleaseEvaluation/summary_tail_and_transcript_continuity`, `TestProcessUsesCommittedSummaryWithRecentVerbatimTail`, `TestServicePlansCompactsAndPreservesRecentTail` |
| Forget, delete, retention, privacy export, account erasure, and linked source/index cleanup behave as documented | `TestV4Issue80ReleaseEvaluation/forget_is_immediate_and_grace_scrubs_content`, `TestPrivacyAuthExportForgetChallengeAndErasure`, `TestPrivacyHardDeletePurgesCanonicalProfileTranscriptAndRevisions` |
| Account merge preserves canonical ownership, admin state, memory, sessions, and encrypted MCP configuration | `TestChallengeMergeTransfersMemoryAndEncryptedMCP`, `TestServiceVerifiedMergePreservesAdminState`, `TestStoreMergeUsersTxReencryptsForWinner` |
| Exact published v3.2 and v3.1.2-upgraded databases reset transactionally and idempotently; unknown schemas fail closed | `TestPublishedV3ResetPreservesIdentityAndMCPAndClearsState`, `TestPublishedV3ResetReopenIsIdempotent`, `TestPublishedV3ResetRejectsMalformedOwnershipWithoutChangingDatabase`, `TestPublishedV3ResetRejectsUnknownStructuralVariant`, `TestPublishedV3ResetRejectsUnknownObjectAlongsideExactVectorFamily` |
| Derived indexes rebuild, publish atomically, and degrade without weakening scope | `TestDerivedIndexOutboxIdempotencyRetryAndRestart`, `TestMaintenanceDuringBuildDoesNotBlockPublication`, `TestIndexMaintenanceRepairsOrphansAndDegradesMissingCoverage` |
| The six builtin model tools and their handlers match the documented inventory | `TestRegisterAdvertisesFinalBuiltinToolNames` |
| Logs have a Loki-ready structured JSON envelope, stable agent correlation, status vocabulary, and redaction | `TestLoggerEnvelopeAndReservedFieldsCannotBeOverwritten`, `TestLoggerAgentFoundationCannotBeOverwritten`, `TestLoggerNormalizesStatusVocabulary`, `TestLoggerSecretPromptAndMCPCanariesAreRedacted` |

`TestV4Issue80ReleaseEvaluation` is a deterministic Go-only application safety gate with eight named scenarios and a required score of 100%. It uses fake extractor/model and embedding behavior, so it validates application policy, persistence, retrieval, delivery, and privacy behavior, not the extraction or answer quality of a live model route. Live-route quality remains an operator evaluation outside this no-services release gate.

The current logging output is structured single-line JSON ready for Loki ingestion. Shipped dashboards and additional periodic memory-quality aggregates are deferred; their absence is not represented as completed observability work.

## Published v3 Reset

The first v4 startup recognizes only the exact published v3.2 schema and the exact schema produced when a v3.1.2 database was upgraded through v3.2, with or without the recognized `memory_entry_vectors` family. It performs one `BEGIN IMMEDIATE` selective destructive reset:

- Preserves canonical user IDs and account timestamps, linked gateway identifiers, display names and verification state, administrator and ban state, and every MCP server column including URL/header ciphertext.
- Rebuilds speaker introductions deterministically from preserved linked-account display names.
- Deletes all user-memory, candidate/evidence/event, profile, session/summary, global-memory, privacy-operation/invalidation, durable-job, derived-index, and WebSocket client/device/bootstrap state.
- Installs only the checksum-frozen `v4_compact_baseline` ledger and verifies ownership, row counts, schema fingerprint, and foreign keys before commit.

Any unknown, modified, experimental, malformed-owner, or partially matching schema fails closed without committing changes. Do not alter a schema to make it match and do not retry against the only copy of a database.

All old FTS and vector objects are deleted. On normal v4 startup, the derived-index worker creates and validates fresh revisions from surviving eligible canonical state; retrieval may report a degraded or unavailable channel until publication. Because the reset removes memory, session, and global canonical rows, their rebuilt indexes are initially empty.

## Upgrade Procedure

1. Record the exact `MCP_CONFIG_ENCRYPTION_KEY` used by the v3 deployment in a separate secrets store. Preserved MCP ciphertext is unusable with another key.
2. Create a WAL-safe backup before starting v4. For a running deployment, use SQLite's online backup API:

```bash
sqlite3 data/database/oswald.db ".backup '/secure-backups/oswald-pre-v4.db'"
```

Alternatively, stop Oswald and copy the database together with its `-wal` and `-shm` companions. Never live-copy only `oswald.db`.

3. Retain the backup unchanged, deploy v4 with the original encryption key, and start Oswald once. A schema-recognition error means startup intentionally refused the reset; restore nothing unless the working copy was otherwise changed.
4. Stop Oswald after the first successful startup and verify the migrated database:

```bash
sqlite3 data/database/oswald.db "PRAGMA integrity_check; PRAGMA foreign_key_check;"
```

`integrity_check` must return exactly `ok`; `foreign_key_check` must return no rows. Restart Oswald and confirm the migration is idempotent, preserved users/links/admin and ban state are correct, MCP servers decrypt and connect with the original key, WebSocket clients must pair again, old memories and sessions are absent, and index revisions begin rebuilding.

## Release Commands

Run the deterministic evaluation and migration gates directly:

```bash
go test -tags sqlite_fts5 ./internal/formationruntime -run '^TestV4Issue80ReleaseEvaluation$' -count=1
go test -tags sqlite_fts5 ./internal/tools/builtin/usermemory -run '^TestOfflineHybridRecallEvaluationCorpus$' -count=1
go test -tags sqlite_fts5 ./internal/database -run '^(TestPublishedV3Reset|TestCompactV4MigrationChecksum)' -count=1
```

Run the full tagged test, build, and vet gates:

```bash
go test -tags sqlite_fts5 ./...
go build -tags sqlite_fts5 -o /tmp/oswald-v4-release ./cmd/agent
go vet -tags sqlite_fts5 ./...
```

Run the focused race gate for formation/publication, merge, privacy, MCP visibility, the agent loop, and delivery:

```bash
go test -race -tags sqlite_fts5 \
  ./internal/formationruntime \
  ./internal/tools/builtin/usermemory \
  ./internal/commands/accountlinking \
  ./internal/privacy \
  ./internal/mcp \
  ./internal/agent \
  ./internal/gateway/runtime \
  -run '^(TestV4Issue80ReleaseEvaluation|TestProposeCandidateAtomicallyPublishesApprovedPolicy|TestProposeCandidateRollsBackEveryPublicationStage|TestChallengeMergeTransfersMemoryAndEncryptedMCP|TestPrivacyAuthExportForgetChallengeAndErasure|TestProviderDiscoveryToolsAreScopedToVisibleEnabledServers|TestProcessExecutesToolThenFinalAnswerAndStreamsEvents|TestExecuteEnqueuesFormationOnlyAfterResponseDelivery)$' \
  -count=1
```

Check the runtime tool inventory and schema files:

```bash
go test -tags sqlite_fts5 ./internal/tools/builtin -run '^TestRegisterAdvertisesFinalBuiltinToolNames$' -count=1
printf '%s\n' data/tools/*.md
```

The expected inventory is exactly `web.search`, `time.current`, `user_memory_search`, `user_memory_list`, `session_transcript_search`, and `global_memory_search`. Also verify `PLAN.md` remains absent:

```bash
test ! -e PLAN.md
```

`PLAN.md` is already absent; issues #74 and #80 and their linked work are the durable release record. CI configuration is not part of this validation change.
