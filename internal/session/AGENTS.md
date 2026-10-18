# session/ — Delivery-Gated Memory, Soul Binding, Durable Compaction

Applies on top of the root `AGENTS.md` (operator-state and persisted-contract invariants).

## Shape

`memory/profile_state_store.go::ProfileStore` owns one trusted profile's `state.db`: session scope,
pending/delivered/failed delivery gating, history paging, the model-usage ledger, image-version
high-water, and maintenance sweeps. `memory/files/store.go` owns the disk `USER.md`/`MEMORY.md`
pair; `soul/store.go` binds read-only policy; `compaction/` (plus `compaction/budget/`) summarizes
delivered turns durably behind the broker's foreground gate. Package names stay `memory` /
`compaction` / `soul` / `budget` / `files`; `session` is only the grouping.

## Delivery gating (the core idea)

Every turn writes all rows **inactive** first: user message, assistant tool requests, tool results,
final assistant message, owning one contiguous `[userMsgID..turnID]` range. `MarkSessionDelivered`
flips the range active; `MarkDeliveryFailed` leaves it inactive and excluded. Reads, compression,
and search all gate on delivered rows only — pending delivery is a barrier everywhere, which is
why late success must invalidate any summary or receipt that may have omitted the repaired turn,
while late failure can never undo success.

`/new` is bookkeeping, not deletion: it ends the open session and bumps the generation high-water
mark in `conversation_generations`. Transcripts, summaries, locks, and receipts stay stored and
searchable until TTL expiry sweeps them (24h inactivity, ≤100 sessions and ≤100 timed-out
exchanges per maintenance pass, 15-minute pending timeout, passive WAL checkpoint).

## Persisted shapes

- `oswald:v1:files:<sid>` (≤32 KiB): first-writer-wins frozen USER/MEMORY pair. Both strings
  required; incomplete snapshots fail closed.
- `oswald:v1:turn:<sid>:<id>` (≤256 KiB): correlated user message ID, bounded native tool history,
  successful tool names, prompt pressure, delivery state, generated-image references.
- `oswald:v1:summary:<sid>` (≤64 KiB) and `oswald:v1:compression:<sid>:<from>:<through>:<contract>`
  (≤64 KiB, at most 4 submissions): durable artifacts with source ranges, retry state, exact leases,
  and provider credits. Tool history as encoded is capped at 64 KiB.
- `sessions.origin_json` (≤8 KiB): `{version:1, generation, ttl, platform/chat/user/profile}`.

Search is a disposable in-memory FTS5 projection over eligible rows — no new persisted index. The
live session and its forks are excluded from discovery and browse; older lineage generations stay
recallable, deduped to one entry per lineage root.

## Soul and files (why two mechanisms)

`SOUL.md` (≤1 MiB, profile file else root fallback) is operator policy the model can never edit;
runtime never copies, migrates, or deletes it. `USER.md` (≤1,375 runes) and `MEMORY.md` (≤2,200
runes, `§`-separated entries) are model-editable through the `memory` tool only: one action or
1–20 ordered ops per atomic batch, replace/remove needing exact unique `old_text`, stable
per-target flock files with re-read, validation, synced temp-file rename, and directory sync.
Capacity errors return current entries for consolidation. No cross-file/SQLite/delivery
transaction exists — that absence is deliberate, and external editors must follow the locks.

Images: at most 4 refs per exchange, 8 per session, and only delivered active-generation refs
load. Cache files expire after 24h and are never reconstructed from SQLite.

## Compaction (`compaction/compactor.go`, `profiles.go`, `budget/`)

Two layers. Foreground compaction checkpoints only validated complete tool rounds inside the turn,
using model-visible arguments and results, omitting reasoning and attachment bytes, and retaining
old context on failure. The durable `ProfileService` runs after delivery and every 30s behind the
foreground-priority gate, pinning a delivered high-water campaign target across partial
checkpoints; retry ranges stay fixed if newer turns arrive.

The model call is silent streaming, temperature 0, no parallel calls, exactly one required
`session_summary_save`: narrative ≤8,000 runes; 5 arrays × 50 items × 1,000 runes; structured
aggregate ≤16,000 runes; encoded artifact ≤40 KiB. Chunks hold at most 64 complete delivered
exchanges and fit the actual model input budget.

Locks are five-minute renewable exact tokens. Save requires a submission to exist and the
`candidates` field to be empty; publication atomically validates the exact live lease, the
complete delivered range with no pending exchange inside it, and unchanged source text. Saved
artifacts survive restart without another credit; preemption refunds only before artifact save.
Oversized complete exchanges get dead receipts, and summaries never delete covered transcript.

## Tests

Cover pending/delivered/failed gating, late-repair invalidation, `/new` generation fencing, file-op
atomicity under competing writers, claim/lease/publication races, and oversize-turn dead receipts.
