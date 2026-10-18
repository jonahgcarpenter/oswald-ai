# Transport - Gateways and Media

`internal/transport/` owns Discord, iMessage, and OpenAI transports plus shared media handling.
`gateway/` is composition; concrete packages depend on `gateway/routing` and `gateway/runtime`,
not the other way around.

## Packages

- `gateway` (`gateway/`) — `Service` contract and `NewServicesFromConfig`: which transports start
  from which config blocks, plus BlueBubbles config validation.
- `discord` (`gateway/discord/`) — inbound admission, mentions/replies, streaming status, FIFO
  outbound delivery, attachments, REST client, labels, cron sender.
- `imessage` (`gateway/imessage/`) — BlueBubbles client, webhook listener and registration,
  inbound admission, chronological thread replies, final-only responses, contacts, labels.
- `openai` (`gateway/openai/`) — stateless bearer-key API: models listing, chat completions with
  SSE, caller-tool round-trips. No commands, no persistence, no attachments.
- `routing` (`gateway/routing/`) — cheap `Decide`/`Preflight`: ignore vs LLM vs command vs
  gateway fallback, before any expensive work.
- `runtime` (`gateway/runtime/`) — `Execute()`: profile fence, group-scope check, command/LLM
  dispatch, broker submission, delivery measurement, compaction enqueue.
- `media` (`media/`) — attachment normalization (resize, orientation-first, contact sheets),
  outbound attachment validation, vision bounds.
- `imagecache` (`media/imagecache/`) — private per-sender cache with HTTPS import, 24h TTL, and
  the shared-prune-worker hook.

## Rules

- Private messages need no mention; group messages and group commands require the mention policy
  or a recognized bot reply, and the mention must lead the message. Transport scope and reply
  context never change sender ownership.
- Sessions: Discord `discord:dm:<author>` vs `discord:<channel>:<author>`; iMessage
  `imessage:dm:<sender>` vs `imessage:<chat>:<sender>`; OpenAI `openai:<id>` stateless.
- Discord checks leading native `<@ID>`/`<@!ID>` tokens; BlueBubbles uses anchored
  `mention_pattern` (default `@?Oswald\b`), stripped before the model call.
- iMessage group threads resolve the newest conversational message strictly before the incoming
  reply in the same chat/thread. Failed thread lookups expose no fallback root context.
- New stored turns keep `[Replying to ...]` enrichment and `[Image attached at: ...]` annotations;
  quoted text stays untrusted reference, never deployment instruction.
- Discord streams status/previews under its 2,000-unit limit; final inventory reconciles streamed
  output. Shared final outbound: 20 entries / 80 MiB, 5-minute admission, 15s attempts, 1–30s
  exponential retry. iMessage sends typing/read indicators and a final-only response.
- Client history on the API is lower-authority quoted reference; caller tools round-trip verbatim
  with `finish_reason: tool_calls`. History sizing is token-only against the caller profile budget.
- Media in: ≤4 images, 10 MiB each (JPEG/PNG/GIF/WebP/HEIC/HEIF), normalized to ≤2560 longest edge
  / 280 KiB. Out: ≤8 MiB each, 10 files / 80 MiB total. `MEDIA:/absolute/path` tokens resolve
  against cache then CWD and are stripped before history and the text-only API.
- Orientation is corrected before resize (EXIF 1–8; HEIF `irot`/`imir` wins without compounding).
  GIF uses the first frame or a ≤4-tile contact sheet; GIFV extracts ≤4 ffmpeg frames.
- Cache files expire after 24h and are never reconstructed from SQLite. Label fetches log only
  statuses and codes — never names or IDs.

## Testing

Cover the admission/mention matrices, reply predecessor selection, ownership isolation, streaming
and outbound limits, attachment denials, orientation handling, and expiry sweeps.
