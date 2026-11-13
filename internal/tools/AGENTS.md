# tools/ — Builtin Registry, Governance, Tool Inventory

Applies on top of the root `AGENTS.md` (never-rename-the-outside-world and never-log-rules).

## Shape

`bootstrap.go` builds `BuiltinDefinitions()` and the config-driven registry
(`NewRegistryWithImageCache`, disabling `image_generate` without ComfyUI, `web_search` without a
backend, `browser_*`/`browser_vault_*` without an endpoint or launch command,
`session_search` without a store). `register.go` wires handlers, policies, and argument
normalizers. `registry/` holds definitions, handlers, policies, and per-visibility catalogs;
`governance/` owns the progress-gated stop rule (loop-guardrail ladders plus a
12-consecutive no-progress window halt stuck requests) with a high safety-valve
cap (500 handler executions, 200 tool-bearing responses per request) as a
runaway backstop, plus result-text framing; `exposure/` computes request-local
visibility; `output/` strips ANSI and redacts secrets (`bearer`, api keys, `sk-`, `ghp_`,
`github_pat_`, `xox[baprs]-`). `toolsets.go` bundles code-owned sets — `core` (memory, skill_manage, search,
web, vision, image, browser, vault, plus file tools), `terminal`, `code_execution`, `cronjob` —
which gateways map to and the API intersects with caller `named`/`allowed_tools`.

## Per-tool contracts

- `memory` — atomic USER/MEMORY entry ops (one action or 1–20 ordered ops; replace/remove need
  exact unique `old_text`); capacity errors return current entries for consolidation.
  Metadata-only history: never searchable.
- `skill_manage` — on-demand procedural skills over the profile `skills/` tree (`skill_manage/`
  leaf package; catalog/index/reads owned by `internal/skills/`): `list` (compact
  name/description/category index, 200-row cap), `view` (full body or one supporting file),
  `create` (slug-validated new `SKILL.md`, or `file_path` supporting file in an existing
  skill), `update`/`remove` (supporting-file shapes via `file_path`; updates keep exact
  unique `old_text` and never bump `version:`, `remove` never takes `SKILL.md` itself;
  plugin skills read-only for every write shape). Installed plugin skill views carry a plugin-runtime footer (bare-name
  invocation, other-systems-docs disclaimer); profile skills and
  supporting-file reads carry none. Unknown arguments rejected; authenticated principal required.
  Metadata-only history with hashed bodies: never searchable.
- `session_search` — full searchable history with four inferred shapes: `query` (FTS5 discovery,
  BM25-ranked, lineage-deduped, default 5), `session_id` + `around_message_id` scroll (1–20 each side),
  `session_id` full read (first 20 / last 10 past 30 rows), no-args recent browse (default 3).
  No `profile` argument: bare ids resolve owner-first across peer stores. In a group turn the
  owner sees their DMs plus the live group while peer stores contribute that group only;
  a group `session_id` merges every member slice chronologically (live slice omitted as
  already in context) with per-message `profile`/`sender`/`scope` attribution, and `link`
  values stay verbatim `@session:<profile>/<id>`. DMs and other groups fail closed as
  `session not found in this group`. Envelopes stay within 14,000 runes via progressive
  reduction that preserves navigation and attribution while marking truncation.
- `web_search` — query ≤400 runes/50 words, ≤100 results requested (default 5), 16 KiB
  model envelope with tracking-stripped destinations.
- `vision_analyze` — question ≤2,000 runes, optional 4-int region, cached-or-URL sources.
- `image_generate` — prompt ≤2,000 runes, fixed landscape/square/portrait ratios, HTTPS or
  cached-cache sources only; previews are request-local, durable history keeps text.
- `browser_*` — eleven local-headless tools (`browser/` leaf package): `navigate` opens a URL
  and returns a compact snapshot; `snapshot`/`click`/`type`/`scroll`/`back`/`press` act by `@eN`
  ref and refresh the snapshot; `get_images` lists non-data-URL page images; `vision` attaches a
  native screenshot (optional annotated boxes); `console` reads buffered console/JS errors or
  evaluates one JS expression; `dialog` accepts/dismisses a blocking native dialog surfaced in
  `pending_dialogs`. Flat `{"success": true, ...}` envelopes inside the untrusted wrapper;
  oversized snapshots truncate at line boundaries with the full tree spilled to
  `cache/browser/scratch-<hash>.txt` for `read_file` paging.
- `browser_vault_*` — six model-blind vault tools over the same session: `list` (handles
  plus metadata, never passwords), `fill` (login password or address fields, exact-origin bound,
  card kinds refuse fail-closed), `save_login` (seals a user-supplied identifier/password/seed
  to the current page origin), `save_address` (seals user-supplied address fields the same way),
  `enter_code` (TOTP from a saved seed, or a save-first hint when none exists), `rm` (exact-handle
  delete, irreversible). There is no vault CLI: chat is the only ingest path, so user-supplied
  secrets transit tool args — metadata-only history, hashed args, secret registry scrubbing on
  every browser output path.
- `clarify` — blocking single-flight question form (1–5 questions, ≤4 choices each).
  Parks one pending ask per conversation (5-minute window) and walks the questions
  one at a time: each goes out as its own text, the user's reply answers it, and the
  next follows. Answers return as this call's tool result
  (`{responses, outcome}` with `submitted/cancelled/timed_out/undelivered`);
  timeouts and cancellations keep answered-so-far. Replies are
  intercepted pre-broker so they never start a turn; `/new` and `/stop` cancel the wait.
  Hidden on the stateless API and unattended transports (fail-closed `undelivered`).
- `delegate` — scoped subagent turns: `spawn` (one goal or 1–10 parallel tasks,
  each with goal/context plus optional output_schema/images, plus `background`
  opt-in) and `list`/`steer`/`stop` over the caller's live background session
  units only. Top-level dispatches run sync by default (blocking tool result);
  `background:true` runs async with a handle receipt and later new-turn
  delivery; nested orchestrator calls run inline. Full history, searchable results.
- `read_file` — paginated text read (`LINE_NUM|CONTENT`, 1-indexed offset, 2000-line
  default/max, ~100K-char budget with `next_offset` continuation); binary containers
  (`.docx/.xlsx/.ppt/.pdf`/Office/OpenDocument/RTF/EPUB/SQLite) auto-extract to text,
  images refuse with a `vision_analyze` redirect; similar-filename suggestions on miss.
  Inbound document cache paths resolve first.
- `write_file` — whole-file replace with read-before-write gate (refuses without a
  current full read or on external modification), syntax-checked (new errors only),
  atomic temp-rename with `verified:<hash>` receipt; parents created private.
- `patch` — targeted find-and-replace (unique match or `replace_all`), 9-strategy
  fuzzy matching with closest-match hint, unified-diff result, same syntax gate.
- `search_files` — ripgrep-backed content search (regex, line numbers, context lines,
  `files_only`/`count` modes, 50-result default) and glob file search (discovery or
  exact newest-first order); skips `.git`/`node_modules`/`__pycache__` and >1 MiB files.
- `terminal` — foreground `/bin/sh -c` (default 180s, 600s max; over-cap promotes to
  tracked background) with minimal env (`PATH` includes `/opt/oswald`,
  `TMPDIR=<cwd>/cache/scratch/`); arg validation lives here, guarded execution in
  `workspace.ExecuteCommand` (pre-execution chain, review escalation, spawn).
- `execute_code` — stateless `python3` per call (300s cap, 200KB code limit, 50KB
  output cap) with `__pycache__` reset support; shares the terminal arg-surface pattern.
- `process_manage` — tracked background sessions: `list|poll|log|wait|kill|write|
  submit|close|handoff` (unique-prefix ids, 200-line log default); stdin writes run
  the confined-write/protected-read scans; `handoff` reparents to the caller's scope.
- `cronjob_manage` — schedules end to end (`create|list|update|pause|resume|remove|run`),
  rejects `model/provider/base_url` overrides; scripts resolve under per-profile
  `scripts/`, delivery targets the concrete `deliver` object.

History policy in one line: `memory`/`skill_manage`/`browser_vault_*` metadata-only, `session_search`/`web_search`
searchable, image-tool and `clarify` text nonsearchable — with 16 KiB args and 16,000 result runes per call
inside a 64 KiB aggregate trace. `session_search` never logs query or message content; browser
and vault tools never log URLs, page content, expressions, or secrets — only counts and chars.

## Tests

Cover advertised schema and governance per tool, the enable/disable matrix, history-visibility
policy, envelope truncation, cancellation, and output redaction.
