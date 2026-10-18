# Session - Memory, Soul, Compaction

`internal/session/` owns conversation persistence and summarization. `memory` holds sessions,
delivery, history, and file edits; `soul` binds read-only policy; `compaction` summarizes delivered
turns durably. Package names stay `memory`/`compaction`/`soul`; `session` is only the grouping.

## Delivery and history

- `memory/profile_state_store.go` — `ProfileStore`: one trusted profile. Pending turns write all
  rows inactive; `MarkSessionTurnDelivered` activates them, `MarkDeliveryFailed` leaves them
  inactive and excluded. Reads, compression, and search gate on delivered rows only. Late
  success invalidates summaries/receipts that may have omitted the turn.
- `/new` (`memory` + `internal/app/AGENTS.md`) only ends the open session and bumps the generation
  high-water mark. Transcripts, summaries, and locks stay until TTL expiry sweeps them.
- `state_meta` records: `oswald:v1:files:<sid>` (frozen USER/MEMORY pair), `oswald:v1:turn:<sid>:<id>`
  (correlated user ID, bounded tool history, pressure, delivery, image refs), summary / compression
  receipt / campaign keys. Tool history cap 64 KiB encoded.
- `memory/profile_search*.go` — disposable in-memory FTS5 projection, lineage dedupe, no new
  persisted index. Live session and forks excluded; older lineage stays recallable.
- `memory/files/store.go` — disk `USER.md` (≤1,375 runes) / `MEMORY.md` (≤2,200 runes), `§`-separated
  entries, 1–20 ordered ops per atomic batch with per-target flock files.
- `soul/store.go` — read-only `SOUL.md`, 1 MiB cap, profile file else root fallback. Runtime never
  copies, migrates, edits, or deletes souls.
- Images: at most 4 refs/exchange, 8/session; only delivered active-generation refs load.

## Compaction

- `compaction/compactor.go` — silent streaming, temperature 0, one required `session_summary_save`
  call. Narrative ≤8,000 runes; 5 arrays × 50 items × 1,000 runes; structured ≤16,000 runes;
  artifact ≤40 KiB. Saved artifacts survive restart; preemption refunds only before artifact save.
- `compaction/profiles.go` — durable worker: enqueue on delivery, 30s cycle behind the broker
  foreground gate. Chunks ≤64 complete delivered exchanges; pending delivery is a barrier.
- `compaction/budget/` — estimator and 70% trigger, 32,768 default window, 256 safety margin,
  recent-tail 2,000–8,000.
- Locks: 5-minute renewable exact tokens. Publication validates live lease, complete delivered
  range, and unchanged source text. Campaigns pin a delivered high-water target.

## Testing

Cover pending/delivered/failed gating, late-repair invalidation, `/new` generation fencing, file-op
atomicity, claim/lease/publication races, and oversize-turn dead receipts.
