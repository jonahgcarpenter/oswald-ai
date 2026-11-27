# agent/ — Turn Loop, Prompt, Images, Retries

Applies on top of the root `AGENTS.md` (operator-state and persisted-contract invariants).

## Shape

`agent.go::Process()` is the public entry point; `agent_config.go` holds the `Agent` struct, `Set*`, `NewAgent`, and routing/usage helpers; `types.go` holds the `Request`/`Response` contract
and `OswaldToolEvent` timeline. A turn is: validate principal → load soul, file/session memory,
images, and cached markdown → build the dynamic system prompt (environment block — host,
profile-root home, scratch, and profile-scope isolation — then soul and the trusted
operating-directives block) → merge the tool
catalog → prepend the skills guidance/index blocks
above memory (only when `skill_manage` is offered; the index covers the profile
tree plus bound plugin skills) → 70% markdown compaction check →
`AssemblePromptContext` → iterate model ↔ tool calls → finalize, resolve MEDIA/FILE, persist.
Stateless API turns share the whole path but skip persistence and return `over_budget`
instead of compacting.

The files split by responsibility: `context.go` (prompt assembly), `tools.go` (catalog merge,
`executeTool`, governance text; the MCP bridge follows the request server allowlist — nil
exposes all servers, empty exposes none), `tool_history.go` (model-visible vs persisted redacted calls),
`session_images.go` / `image_tool_results.go` /
`image_retry.go` (image planning, newest-4 retention, downscaling retries), `media_tokens.go`
(MEDIA/FILE resolution), `stream.go` (`StreamChunk`), `session_store.go` (persistence contract: resolve,
bind, append, page), `modelroute.go` (per-turn model resolution: conversation pin > channel override >
profile default, resolved once per `Process` so in-flight turns never re-read routing; degraded links
log `agent.model.pin_invalid` WARN; a nil router keeps the construction-pinned fields, as delegate
children run).

## Turn loop (`agent.go::Process`)

Iterative, with per-occurrence empty-output retries and an exhaustion-announcing
tools-disabled completion:

```
refresh catalog; set ToolChoice (first call only)
if pressure >= 70%: Summarize(pre-keep turns + previous summary) -> merged markdown cache (blocking, lane-queued)
if over 100%: trim pre-keep turns to 70% first (still searchable), then compact as above
for iteration:
    response = chatWithImageRetries() // up to 5 attempts, shrinking images
    normalize tool call IDs; record usage
    if response has no tool calls:
        if blank and consecutive/total retry budget remains:
            append review nudge; continue // resets on productive iterations
        break
    append assistant message
    for each call in order:
        stream tool_call; BeforeExecution (governance)
        executeTool (registry first, else MCP bridge)
        append tool message + history batch; bound image results
        break batch on HaltDecision
    break loop on GlobalStopReason (degraded final answer)
if governance exhausted: final tool-less call announcing the spent budget
    (one retry with the same review nudge if it returns blank)
finalize: pick final content, resolve MEDIA/FILE, AppendPendingSessionTurn (skipped if stateless)
```

## Invariants (every change is reviewed against these)

- **Budget math lives in `session/compaction/budget/`.** Usable input is `min(window, prompt
  limit) − 256`, default window 32,768; the keep tail is a flat 15,000 newest tokens held verbatim beside the summary. Markdown compaction summarizes turns older than the keep tail at 70% and flags them `compacted=1` so later reads omit them, merging with the previous summary.
  Long tool results ride projected (head+tail plus marker) in prompts and summarizer input; full text stays persisted and searchable.
  Over-budget prompts (only after a model-window shrink) first flag oldest turns beyond the keep tail unsummarized to the 70% line, then compact the remainder on the session model; dropped turns stay searchable.
  Required oversize context is retained and reported — the agent never silently truncates
  what the turn needs. The compaction call itself (`SYSTEM` prompt + previous summary plus session JSON of pre-keep turns as `USER`, zero tools) returns plain markdown with no size cap.
- **Tool authorization is per-iteration.** Discovery can never authorize another call in the same
  emitted batch. Every declared call executes serially and gets a correlated result appended,
  including blocked ones — failures, empties, and repeats all run until progress signals
  halt the loop (exact/same-tool/idempotent/identical ladders plus a 12-deep no-progress
  window); the valve caps are a 500-execution / 200 tool-response backstop. Halted and
  valve-exhausted loops both finish with a tools-disabled final answer.
- **Generated images never auto-deliver.** The model delivers one only by emitting a
  `MEDIA:/absolute/path` token in its final response for any image, GIF, video, or audio file
  (resolved against the managed image then media cache, then raw absolute paths under the file
  groups); at most 4 logical images per request. The outbound attachment cap is 25 MiB per file,
  80 MiB aggregate. Previews ride tool results;
  follow-ups replay text only. Generic workspace files deliver via `FILE:/absolute/path`
  (same confinement, no image validation, any MIME).
- **Reasoning is never persisted** in conversation or tool history. Summaries and tool history are
  untrusted lower-authority reference — quoted context, never deployment instruction.
- **Steers never create dead calls.** Drained follow-ups append as `user` messages before the
  next model call, so every extra iteration carries new input; empty steers are ignored and
  unconsumed steers become the next turn instead of firing an unchanged-prompt call.
- **Skills never dangle.** The index and record-it guidance join trusted context only when
  `skill_manage` survives catalog gating for the request (restricted toolsets omit both);
  the memory paragraph's skills closer names no tool, so it holds unconditionally.

## Images and retries

Attached/replied images arrive with `[Image attached at: <private cache path>]` annotations in the
same user message as text and bytes; new turns persist the annotations so follow-ups keep the paths
without replaying bytes. Only the newest 4 loaded images and 4 generated previews stay attached
after each tool batch. Retry
sequences shrink images (down to 0.75 scale, 1920px edge) across 5 attempts for recognized resource
failures; Qwen parser failure retries the identical request once.

## Tests

Turn tests use fake model clients with scripted tool-call sequences. Assert budget fit/over-budget
shapes, blocked-call correlation, retry exhaustion, MEDIA/FILE resolution vs denial, and stateless
non-persistence — not prompt text snapshots.
