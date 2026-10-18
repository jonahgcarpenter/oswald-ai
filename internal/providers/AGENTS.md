# Providers - Model and Backend Clients

`internal/providers/` owns backend implementations. `llm` is the model contract; `web` and
`image_generate` are backends. Tool packages never get imported from here.

## Layout

- `llm/` — `Chat`/`Embed` plus async poll and SSE streaming, wire DTOs, public types, usage
  telemetry. Tool wire names derive from correlated invocation IDs; `required:[]` is always
  emitted explicitly.
- `web/` — shared `Searcher` contract, normalization, Brave-primary/SearXNG-fallback.
  `web/brave/` posts the LLM-context endpoint (8 URLs, ~3,072-token budget, 30s attempts, 50/sec
  rolling limit). `web/searxng/` queries first-page JSON (8s attempts, same-origin redirects).
- `image_generate/comfy_ui/` — dedicated serialized client: fixed SD3.5 Large Turbo graph
  (UNet + dual CLIP + VAE, shift 3, euler/sgm_uniform, 4 steps, CFG 1, fresh seed), single-flight
  permit, `/free` model unload, 10s detached cleanup.

## Rules

- Query bounds: ≤400 runes, ≤50 words, valid UTF-8, no control chars. Provider bodies ≤2 MiB;
  first 50 candidates normalized to ≤50 results, max 2 per hostname, 16 KiB model envelope.
  Retry once for explicit 408, eligible short-window 429, and selected 5xx; never retry ambiguous
  metered or malformed successes. Clean empty fallback is not degraded.
- ComfyUI: request controls only prompt, ratio (1280×720 / 1024² / 720×1280), optional source and
  denoise (default .75, range 0.1–0.9). Output PNG/JPEG/GIF/WebP ≤8 MiB, dims ≤8192, MIME and
  decode-config validated.
- Timeouts start after caller-cancelable permit acquisition. Upstream cancellation always
  interrupts provider work.
- Never log queries, URLs, bodies, headers, credentials, or secret material.

## Testing

Cover wire-shape fidelity, retry classifiers, fallback/degraded accounting, graph validation,
timeout/cleanup behavior, and output bounds with local HTTP handlers and fake servers.
