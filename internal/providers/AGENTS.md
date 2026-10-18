# Providers - Model and Backend Clients

`internal/providers/` owns backend implementations. `llm` is the model contract; `web` and
`image_generate` are backends. Tool packages never get imported from here.

## Packages

- `llm` (`llm/`) — Bifrost/OpenAI-compatible model client: `Chat`/`Embed`, async submit/poll,
  SSE streaming, wire DTOs, public request/response types, usage telemetry, retry classifiers.
- `web` (`web/`) — shared `Searcher` contract, query/result normalization and bounds, plus the
  Brave-primary/SearXNG-fallback selector with degraded accounting.
- `brave` (`web/brave/`) — Brave LLM-context endpoint client with rolling rate limiter.
- `searxng` (`web/searxng/`) — SearXNG first-page JSON client for private deployments.
- `comfy_ui` (`image_generate/comfy_ui/`) — dedicated serialized ComfyUI client: fixed SD3.5
  Large Turbo graph, single-flight permit, history polling, output validation, `/free` cleanup.

## Rules

- Query bounds: ≤400 runes, ≤50 words, valid UTF-8, no control chars. Provider bodies ≤2 MiB;
  first 50 candidates normalize to ≤50 results, max 2 per hostname, 16 KiB model envelope.
  Retry once for explicit 408, eligible short-window 429, and selected 5xx; never retry ambiguous
  metered or malformed successes.
- ComfyUI: the request controls only prompt, ratio (1280×720 / 1024² / 720×1280), optional source,
  and denoise (default .75, range 0.1–0.9). Output PNG/JPEG/GIF/WebP ≤8 MiB, dims ≤8192, MIME and
  decode-config validated.
- Timeouts start after caller-cancelable permit acquisition. Upstream cancellation always
  interrupts provider work.
- Never log queries, URLs, bodies, headers, or credentials.

## Testing

Cover wire-shape fidelity, retry classifiers, fallback/degraded accounting, graph validation,
timeout/cleanup behavior, and output bounds with local HTTP handlers and fake servers.
