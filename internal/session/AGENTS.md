# Session - Memory, Soul, Compaction

`internal/session/` owns conversation persistence and summarization. `memory` holds sessions,
delivery, history, and file edits; `soul` binds read-only policy; `compaction` summarizes delivered
turns durably. `session` itself is only the grouping.

## Packages

- `memory` (`memory/`) — `ProfileStore`: session scope, pending/delivered/failed delivery gating,
  history paging, FTS5 recall projection, model-usage ledger, image-version high-water, summary
  artifacts, maintenance sweeps.
- `files` (`memory/files/`) — disk `USER.md`/`MEMORY.md` edits: atomic batches with per-target
  flock files, never a cross-file/SQLite transaction.
- `compaction` (`compaction/`) — synchronous model compactor (`session_summary_save` tool) plus
  the durable per-profile worker behind the broker foreground gate.
- `budget` (`compaction/budget/`) — token estimator, usable-input limit, recent-tail allowance,
  70% trigger.
- `soul` (`soul/`) — read-only `SOUL.md` binding with root fallback; never creates, migrates, or
  deletes.

## Rules

- Pending turns write all rows inactive; `MarkSessionDelivered` activates them, `MarkDeliveryFailed`
  leaves them inactive and excluded. Reads, compression, and search gate on delivered rows only.
  Late success invalidates summaries/receipts that may have omitted the turn.
- `/new` only ends the open session and bumps the generation high-water mark. Transcripts,
  summaries, and locks stay until TTL expiry sweeps them.
- `state_meta` records: `oswald:v1:files:<sid>` (frozen USER/MEMORY pair), `oswald:v1:turn:<sid>:<id>`
  (correlated user ID, bounded tool history, pressure, delivery, image refs), summary / compression
  receipt / campaign keys. Tool history cap 64 KiB encoded.
- Search uses a disposable in-memory FTS5 projection with lineage dedupe, not a new persisted
  index. The live session and its forks are excluded; older lineage stays recallable.
- `USER.md` ≤1,375 runes, `MEMORY.md` ≤2,200 runes, `§`-separated entries, 1–20 ordered ops per
  atomic batch. `SOUL.md` ≤1 MiB.
- Images: at most 4 refs/exchange, 8/session; only delivered active-generation refs load.
- Compaction: silent streaming, temperature 0, one required `session_summary_save` call. Narrative
  ≤8,000 runes; 5 arrays × 50 items × 1,000 runes; structured ≤16,000 runes; artifact ≤40 KiB.
  Chunks hold ≤64 complete delivered exchanges; pending delivery is a barrier.
- Locks are 5-minute renewable exact tokens. Publication validates live lease, complete delivered
  range, and unchanged source text. Campaigns pin a delivered high-water target.

## Testing

Cover pending/delivered/failed gating, late-repair invalidation, `/new` generation fencing,
file-op atomicity, claim/lease/publication races, and oversize-turn dead receipts.
