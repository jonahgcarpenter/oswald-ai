# session/ — Delivery-Gated Memory, Soul Binding, Markdown Compaction

Applies on top of the root `AGENTS.md` (operator-state and persisted-contract invariants).

## Shape

`memory/profile_state_store.go::ProfileStore` owns one trusted profile's `state.db`: session scope,
pending/delivered/failed delivery gating, history paging, the model-usage ledger, image-version
high-water, and maintenance sweeps. `memory/files/store.go` owns the disk `USER.md`/`MEMORY.md`
pair; `soul/store.go` binds read-only policy; `compaction/` (plus `compaction/budget/`) summarizes
delivered turns to markdown in the agent lane. Package names stay `memory` /
`compaction` / `soul` / `budget` / `files`; `session` is only the grouping.

## Delivery gating (the core idea)

Every turn writes all rows **inactive** first: user message, assistant tool requests, tool results,
final assistant message, owning one contiguous `[userMsgID..turnID]` range. `MarkSessionDelivered`
flips the range active; `MarkDeliveryFailed` leaves it inactive and excluded. Reads, compression,
and search all gate on delivered rows only — pending delivery is a barrier everywhere, which is
why late success must invalidate any summary or receipt that may have omitted the repaired turn,
while late failure can never undo success. Compaction additionally flags summarized ranges
`compacted=1` (`MarkSessionTurnsCompacted`); context reads exclude flagged rows while search
keeps finding them.

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

## Compaction (`compaction/summarizer.go`, `budget/`)

70% of the usable input triggers one blocking markdown call inside the lane head:
`SYSTEM=compaction prompt`, `USER=session JSON dump of all but the newest 3 turns`,
zero tools, plain markdown out. On success the summarized message ranges are flagged
`messages.compacted=1` (newest 3 stay verbatim) and the markdown is cached in memory
per session generation (no DDL, no size cap; restart recomputes from the transcript).
Context reads exclude `compacted=1` rows, so prompt assembly is
`system + markdown-user + non-compacted turns (newest-first tail) + current`.
`/compact` forces the same call (no-op at ≤3 turns); the provider-overflow retry shares
it. `ProfileService` only marks delivery (`Enqueue`/`MarkDeliveryFailed`); no worker,
lease, campaign, or low-priority gate remains. Old `oswald:v1:summary/compression`
rows are ignored, never migrated or deleted.

## Tests

Cover pending/delivered/failed gating, late-repair invalidation, `/new` generation fencing, file-op
atomicity under competing writers, claim/lease/publication races, and oversize-turn dead receipts.
