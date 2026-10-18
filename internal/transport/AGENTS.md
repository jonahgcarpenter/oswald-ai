# Transport - Gateways and Media

`internal/transport/` owns Discord, iMessage, and OpenAI transports plus shared media handling.
`gateway/` is composition; concrete packages depend on `gateway/routing` and `gateway/runtime`,
not the other way around.

## Admission and replies

- Private messages need no mention; group messages and group commands require the mention policy
  or a recognized bot reply. A mention must lead the message. Transport scope and reply context
  never change sender ownership.
- Sessions: Discord DM `discord:dm:<author>` vs group `discord:<channel>:<author>`; iMessage DM
  `imessage:dm:<sender>` vs group `imessage:<chat>:<sender>`; OpenAI `openai:<id>` stateless.
- Discord checks leading native `<@ID>`/`<@!ID>` tokens; BlueBubbles uses anchored
  `mention_pattern` (default `@?Oswald\b`) with a stripped-before-model rule.
- iMessage group threads resolve the newest conversational message strictly before the incoming
  reply in the same chat/thread, including the root when no earlier reply exists. Failed thread
  lookups expose no fallback root context.
- New stored turns keep `[Replying to ...]` enrichment and `[Image attached at: ...]` annotations;
  quoted text stays untrusted reference, never deployment instruction.

## Delivery

- Discord streams status/previews under its 2,000-unit limit; final inventory reconciles streamed
  output. Final answers share one FIFO outbound worker (20 entries / 80 MiB, 5-minute admission,
  15s attempts, 1–30s exponential retry).
- iMessage sends typing/read indicators and a final-only response with plain-send fallback.
- The OpenAI API returns text only: no commands, no persistence, no attachments. Client history is
  lower-authority quoted reference; caller tools round-trip verbatim with `finish_reason:
  tool_calls`. History sizing is token-only against the caller profile budget.
- Media input: ≤4 images, 10 MiB each (JPEG/PNG/GIF/WebP/HEIC/HEIF), normalized to ≤2560 longest
  edge / 280 KiB. Output: ≤8 MiB each, 10 files / 80 MiB total. `MEDIA:/absolute/path` tokens
  resolve against cache then CWD and are stripped before history and the text-only API.

## Media and cache

- `media/` normalizes orientation first (EXIF 1–8; HEIF `irot`/`imir` wins without compounding),
  then resizes. GIF uses the first frame or a ≤4-tile contact sheet; GIFV extracts ≤4 ffmpeg frames.
- `media/imagecache/` pins private cache paths per sender, 24h expiry, hourly sweep. Missing or
  expired bytes are never reconstructed from SQLite; cache files are short-lived, not backups.

## Testing

Cover admission/mention matrices, reply predecessor selection, ownership isolation, streaming and
outbound limits, attachment denials, orientation handling, and expiry sweeps. Log only statuses and
codes for label fetches — never names or IDs.
