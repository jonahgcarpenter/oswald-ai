# tools/ — Builtin Registry, Governance, Tool Inventory

Applies on top of the root `AGENTS.md` (never-rename-the-outside-world and never-log-rules).

## Shape

`bootstrap.go` builds `BuiltinDefinitions()` and the config-driven registry
(`NewRegistryWithImageCache`, disabling `image_generate` without ComfyUI, `web_search` without a
backend, `session_search` without a store). `register.go` wires handlers, policies, and argument
normalizers. `registry/` holds definitions, handlers, policies, and per-visibility catalogs;
`governance/` owns the global caps (50 handler executions, 30 tool-bearing responses per request)
plus the warn/block/halt ladders and result-text framing; `exposure/` computes request-local
visibility; `output/` strips ANSI and redacts secrets (`bearer`, api keys, `sk-`, `ghp_`,
`github_pat_`, `xox[baprs]-`). `toolsets.go` bundles code-owned sets — `core` (memory, search,
web, vision, image, plus file tools), `terminal`, `code_execution`, `cronjob` — which gateways map
to and the API intersects with caller `named`/`allowed_tools`.

## Per-tool contracts

- `memory` — atomic USER/MEMORY entry ops (one action or 1–20 ordered ops; replace/remove need
  exact unique `old_text`); capacity errors return current entries for consolidation.
  Metadata-only history: never searchable.
- `session_search` — full searchable history with four inferred shapes: `query` (FTS5 discovery,
  BM25-ranked, lineage-deduped), `session_id` + `around_message_id` scroll (1–20 each side),
  `session_id` full read (first 20 / last 10 past 30 rows), no-args recent browse (default 3).
  Reads are profile-scoped and delivery-agnostic except the live session and its forks; `link`
  values are verbatim `@session:<owner>/<id>`. Envelopes stay within 14,000 runes via progressive
  reduction that preserves navigation and marks truncation.
- `web_search` — query ≤400 runes/50 words, ≤100 results requested (default 5), 16 KiB
  model envelope with tracking-stripped destinations.
- `vision_analyze` — question ≤2,000 runes, optional 4-int region, cached-or-URL sources.
- `image_generate` — prompt ≤2,000 runes, fixed landscape/square/portrait ratios, HTTPS or
  cached-cache sources only; previews are request-local, durable history keeps text.

History policy in one line: `memory` metadata-only, `session_search`/`web_search` searchable,
image-tool text nonsearchable — with 16 KiB args and 16,000 result runes per call inside a 64 KiB
aggregate trace. `session_search` never logs query or message content.

## Tests

Cover advertised schema and governance per tool, the enable/disable matrix, history-visibility
policy, envelope truncation, cancellation, and output redaction.
