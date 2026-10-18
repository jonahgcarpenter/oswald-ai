# providers/ — Model Contract and Backend Clients

Applies on top of the root `AGENTS.md` (never-log-rules especially: queries, URLs, bodies,
headers, credentials). Tool packages must never be imported from here.

## Shape

`llm/` is the Bifrost/OpenAI-compatible model contract: `Chat`/`Embed` plus async submit/poll,
SSE streaming (`data:` lines, 1 MiB scanner buffer, index-sorted `delta.tool_calls`, `[DONE]`
terminates), wire DTOs (`gateway_wire.go`), public request/response types, and usage telemetry
with retry classifiers (`IsPermanentChatProviderError` for 4xx minus 408/425/429,
`IsTemporaryOllamaToolParserError`, `IsContextLengthExceededError`). Auth is bearer plus virtual
key. Tool wire names derive from correlated invocation IDs, and `required:[]` is always emitted
explicitly — empty is not omitted.

`web/` holds the shared `Searcher` contract, query/result normalization, and the
Brave-primary/SearXNG-fallback selector. `web/brave/` posts the LLM-context endpoint (8 URLs,
~3,072-token budget, 30s attempts, 50/sec rolling limiter, retry only when the 429 burst resets
within a second). `web/searxng/` queries first-page JSON with 8s attempts and same-origin
redirects. `image_generate/comfy_ui/` is a dedicated serialized client (single-flight permit,
250ms history polling, `/free` model unload with 10s detached cleanup) driving a fixed SD3.5
Large Turbo graph: pinned UNet, dual CLIP, VAE, shift 3, euler/sgm_uniform, 4 steps, CFG 1, fresh
seed per generation.

## Bounds that bite

Queries: ≤400 runes, ≤50 words, valid UTF-8, no control chars. Provider bodies ≤2 MiB; the first
50 candidates normalize to at most 50 results, max 2 per hostname, 16 KiB model envelope, with
localhost/private/link-local destinations dropped and tracking params stripped. Retry once for
explicit 408, eligible short-window 429, and selected 5xx — never for ambiguous metered or
malformed successes; a clean empty fallback is not degradation.

ComfyUI requests control only prompt, ratio (1280×720 / 1024² / 720×1280), optional source, and
denoise (default .75, range 0.1–0.9, image-edit only). Output is PNG/JPEG/GIF/WebP ≤8 MiB with
dims ≤8192 and MIME plus decode-config validated. Timeouts start after caller-cancelable permit
acquisition, and upstream cancellation always interrupts provider work.

## Tests

Cover wire-shape fidelity, retry classifiers, fallback/degraded accounting, graph validation, and
timeout/cleanup behavior with local HTTP handlers and fake servers — never live credentials.
