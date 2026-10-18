# Tools - Registry and Builtins

`internal/tools/` owns builtin composition. Each group owns its name, description, schema,
handler, and policy; `registry`, `exposure`, and `governance` stay independent.

## Layout

- `bootstrap.go`, `register.go` — `BuiltinDefinitions()`, `NewRegistryWithImageCache`,
  handler wiring, conditional disable (`image_generate` without ComfyUI, `web_search` without a
  backend, `session_search` without a store).
- `registry/` — definitions, handlers, policies, per-visibility catalogs.
- `governance/` — global caps (50 executions, 30 tool-bearing responses), warn/block/halt ladders,
  result-text framing. `exposure/` — request-local visibility. `output/` — ANSI stripping and
  secret redaction for command output.
- `toolsets.go` — code-owned bundles: `core` (memory, search, web, vision, image + file tools),
  `terminal`, `code_execution`, `cronjob`. Gateways map to toolsets; the API intersects with caller
  `named`/`allowed_tools`.
- Tool groups: `memory` (metadata history), `session_search` (full searchable history),
  `web_search` (full history), `image_generate` + `vision_analyze` (full nonsearchable text history;
  file/shell tools live in `internal/workspace/AGENTS.md`).

## Rules

- History policy: `memory` metadata-only; `session_search`/`web_search` searchable;
  image-tool text nonsearchable. Bounds: 16 KiB args/call, 16,000 result runes/call, 64 KiB
  aggregate trace. `session_search` envelopes stay within 14,000 runes with progressive truncation.
- `session_search` shapes: `query` (FTS5 discovery), `session_id` + `around_message_id` scroll
  (1–20 each side), `session_id` full read, no-args recent browse. Reads are profile-scoped and
  delivery-agnostic except the live session and its forks. `link` values are verbatim
  `@session:<owner>/<id>`. Never log query or message content.
- `web_search`: query ≤400 runes/50 words, ≤100 results requested (default 5), 16 KiB model
  envelope, tracking/fragment-stripped destinations, Brave primary with SearXNG fallback.
- `memory`: one action or 1–20 ordered ops per batch; add needs content, replace/remove need exact
  unique `old_text`. Capacity errors return current entries for consolidation.
- `vision_analyze`: question ≤2,000 runes, optional 4-int region. `image_generate`: prompt ≤2,000
  runes, fixed ratios. Follow the area bounds; never persist image bytes or reasoning.

## Testing

Cover advertised schema/governance per tool, enable/disable matrix, history-visibility policy,
envelope truncation, cancellation, and output redaction.
