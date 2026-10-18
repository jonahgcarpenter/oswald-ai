# Tools - Registry and Builtins

`internal/tools/` owns builtin composition. Each group owns its name, description, schema,
handler, and policy; `registry`, `exposure`, and `governance` stay independent.

## Packages

- `tools` (root) — `BuiltinDefinitions()`, registry construction from config, handler wiring,
  conditional disable, code-owned `toolsets.go` bundles.
- `registry` — definitions, handlers, policies, per-visibility catalogs.
- `governance` — global caps (50 executions, 30 tool-bearing responses), warn/block/halt ladders,
  result-text framing.
- `exposure` — request-local visibility (`HiddenBuiltins`, MCP `ExposeTools`).
- `output` — ANSI stripping and fixed-shape secret redaction for command output.
- `memory` — the `memory` tool: atomic USER/MEMORY entry ops, metadata-only history.
- `session_search` — full searchable history: FTS5 discovery, scroll windows, cross-profile
  `@session:` links, bounded envelopes.
- `web_search` — Brave-primary/SearXNG-fallback search with normalized, bounded envelopes.
- `vision_analyze` — question + cached/URL image analysis; text-only durable history.
- `image_generate` — prompt/ratio/source generation; previews are request-local.

## Rules

- Toolsets are code-owned: `core` (memory, search, web, vision, image + file tools), `terminal`,
  `code_execution`, `cronjob`. Gateways map to toolsets; the API intersects with caller
  `named`/`allowed_tools`.
- History policy: `memory` metadata-only; `session_search`/`web_search` searchable; image-tool
  text nonsearchable. Bounds: 16 KiB args/call, 16,000 result runes/call, 64 KiB aggregate trace.
  `session_search` envelopes stay within 14,000 runes with progressive truncation.
- `session_search` shapes: `query` discovery, `session_id` + `around_message_id` scroll (1–20 each
  side), `session_id` full read, no-args recent browse. Reads are profile-scoped; the live session
  and its forks are excluded. `link` values are verbatim `@session:<owner>/<id>`.
- `web_search`: query ≤400 runes/50 words, ≤100 results requested (default 5), 16 KiB envelope,
  tracking-stripped destinations.
- `memory`: one action or 1–20 ordered ops per batch; replace/remove need exact unique `old_text`.
  Capacity errors return current entries for consolidation.
- `vision_analyze`: question ≤2,000 runes, optional 4-int region. `image_generate`: prompt ≤2,000
  runes, fixed ratios. Never persist image bytes or reasoning, and never log query or message
  content.

## Testing

Cover advertised schema/governance per tool, the enable/disable matrix, history-visibility policy,
envelope truncation, cancellation, and output redaction.
