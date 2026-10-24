# transport/ — Gateways, Admission, Media Normalization

Applies on top of the root `AGENTS.md` (profile-isolation and never-log-rules).

## Shape

`gateway/gateway.go` is the `Service` contract and `gateway/bootstrap.go::NewServicesFromConfig`
decides which transports start from which config blocks (OpenAI on a listen port, Discord on a
token, iMessage on BlueBubbles settings, with port/URL validation). Concrete packages depend on
`gateway/routing` and `gateway/runtime`, never the reverse: `routing/` makes the cheap
ignore-vs-LLM-vs-command decision before any expensive work, and `runtime/Execute()` runs the
profile fence, group-scope check, command/LLM dispatch, broker submission, and delivery marking.
Per-transport wires: `discord/` (inbound, replies, streaming status, FIFO outbound, REST, labels,
cron sender), `imessage/` (BlueBubbles client, webhook listener + registration, multipart
coalescing, link-preview filtering, chronological thread replies, final-only responses, startup
typing sweep), `openai/` (stateless bearer-key API). `media/` normalizes
attachments and `media/imagecache/` pins private cache paths with a 24h TTL hook.

## Admission (cheap before expensive)

Private messages need no mention; group messages and group commands require the mention policy or
a recognized bot reply — and the mention must lead the message (leading whitespace trimmed).
Discord checks leading native `<@ID>`/`<@!ID>` tokens; BlueBubbles uses anchored
`mention_pattern` regexes (default `@?Oswald\b`), stripped before the model call.
`require_mention` (default true) gates group invocation only. Transport scope and reply context
never change sender ownership, and admission (bans first, then allow-list, then mention check)
runs before attachment downloads and reply lookups.

Sessions derive from transport identity: Discord `discord:dm:<author>` vs
`discord:<channel>:<author>`; iMessage `imessage:dm:<sender>` vs `imessage:<chat>:<sender>`;
OpenAI `openai:<id>`, always stateless. Conversation labels fed to the system prompt are
untrusted metadata: DMs use `"DM with <display name>"`, groups fetch the channel/chat name (cached
one hour, degrading to `"group channel"`/`"group chat"` on failure). Label fetches log only
status codes — never names or IDs.

## Replies

iMessage group threads resolve the newest conversational message strictly before the incoming
reply in the same chat/thread — including the root when no earlier reply exists — and neither
cached authorship nor webhook `replyToGuid` associations override that chronological choice. An
unmentioned group reply invokes only when that predecessor is an eligible bot message. Failed
thread lookups expose no fallback root context: chronological continuation, not exact UI-target
proof. New stored turns keep `[Replying to ...]` enrichment and `[Image attached at: ...]`
annotations alongside incoming text, so history retains what was answered; quoted text stays
untrusted reference, never deployment instruction. Link-unfurl preview payloads are dropped from
both inbound and reply contexts (the URL text is kept) so the agent fetches link content itself
instead of receiving unvalidated preview media.

The OpenAI API is text-only: no commands (a leading `/` is a pre-runtime `400`), no persistence,
no attachments. Client history is lower-authority quoted reference, and caller-owned tool calls
round-trip verbatim with `finish_reason: tool_calls` for the external app to run.

## Delivery

Discord streams status and previews under its 2,000-unit limit while a final inventory reconciles
streamed output; all final answers share one FIFO outbound worker (20 entries / 80 MiB, five-minute
admission, 15s attempts, 1–30s exponential retry with numeric Retry-After able to exceed the cap).
iMessage sends typing/read indicators and a final-only response with plain-send fallback; the
webhook route is operator-configurable and registration degrades to WARN, never a startup failure.
iMessage splits one user send into per-part webhooks (text, attachment, link preview), so rapid
parts from one sender/chat coalesce into a single turn (1000ms quiet / 3000ms max hold, guid
dedupe, creation-time ordering) before admission. Startup best-effort clears stale typing
indicators via a bounded chat sweep.

Media in: at most 4 images, 10 MiB each (JPEG/PNG/GIF/WebP/HEIC/HEIF), normalized to ≤2560 longest
edge / 280 KiB JPEG/PNG. GIFs use the first frame or a ≤4-tile contact sheet; GIFV extracts up to
4 ffmpeg frames. Orientation is corrected before resize — all eight EXIF orientations for
JPEG/EXIF-bearing PNG/WebP, ordered `irot`/`imir` for HEIC/HEIF with container transforms winning
so pixels are never double-rotated. Out: ≤8 MiB each, 10 files / 80 MiB total, safe unique
basenames. `MEDIA:/absolute/path` (images) and `FILE:/absolute/path` (any file type)
tokens resolve against the sender's cache first (MEDIA only), then raw absolute paths
under the file groups (protected names sealed, managed cache via the cache resolver), and are
stripped before history and the text-only API.

## Limitations

Cache files expire after 24 hours and are never reconstructed from SQLite — expired previews stay
expired. Source decoding precedes resize, but the bounds don't guarantee strict peak decoded-memory
limits. There are no general file/audio/video inputs, and gateway tool previews stay suppressed:
follow-ups use text-only tool history.

## Tests

Cover the admission/mention matrices per transport, the reply-predecessor selection (including the
root case and corrupt/retracted rows), ownership isolation, streaming and outbound limits,
attachment denials, orientation handling, and expiry sweeps — with fake transports, never live
credentials.
