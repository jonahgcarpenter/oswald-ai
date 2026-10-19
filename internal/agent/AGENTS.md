# agent/ — Turn Loop, Prompt, Images, Retries

Applies on top of the root `AGENTS.md` (operator-state and persisted-contract invariants).

## Shape

`agent.go::Process()` is the public entry point; `types.go` holds the `Request`/`Response` contract
and `OswaldToolEvent` timeline. A turn is: validate principal → load soul, file/session memory,
images, and cached markdown → build the dynamic system prompt (soul + trusted
operating-directives block) → merge the tool catalog → 70% markdown compaction check →
`AssemblePromptContext` → iterate model ↔ tool calls → finalize, resolve MEDIA, persist.
Stateless API turns share the whole path but skip persistence and return `over_budget`
instead of compacting.

The files split by responsibility: `context.go` (prompt assembly), `tools.go` (catalog merge,
`executeTool`, governance text), `tool_history.go` (model-visible vs persisted redacted calls),
`session_images.go` / `image_tool_results.go` /
`image_retry.go` (image planning, newest-4 retention, downscaling retries), `media_tokens.go`
(MEDIA resolution), `stream.go` (`StreamChunk`), `session_store.go` (persistence contract: resolve,
bind, append, page).

## Turn loop (`agent.go::Process`)

Iterative, with a one-shot empty-output retry and a final tools-disabled completion:

```
refresh catalog; set ToolChoice (first call only)
if pressure >= 70%: Summarize(full session JSON) -> markdown cache (blocking, lane-queued)
for iteration:
    response = chatWithImageRetries() // up to 5 attempts, shrinking images
    normalize tool call IDs; record usage
    if response has no tool calls: break
    append assistant message
    for each call in order:
        stream tool_call; BeforeExecution (governance)
        executeTool (registry first, else MCP bridge)
        append tool message + history batch; bound image results
        break batch on HaltDecision
    break loop on GlobalStopReason (degraded final answer)
if governance exhausted: one final tool-less model call
finalize: pick final content, resolve MEDIA, AppendPendingSessionTurn (skipped if stateless)
```

## Invariants (every change is reviewed against these)

- **Budget math lives in `session/compaction/budget/`.** Usable input is `min(window, prompt
  limit) − 256`, default window 32,768; recent-tail reserve is 25% of input clamped to
  2,000–8,000, holding at most the two newest turns; markdown compaction triggers at 70%.
  Required oversize context is retained and reported — the agent never silently truncates
  what the turn needs. The compaction call itself (`SYSTEM` prompt + full session JSON as
  `USER`, zero tools) returns plain markdown with no size cap.
- **Tool authorization is per-iteration.** Discovery can never authorize another call in the same
  emitted batch. Every declared call executes serially and gets a correlated result appended,
  including blocked ones — failures, empties, and repeats all run, with loop-guardrail warnings
  advising instead of refusing. Caps are 50 handler executions and 30 tool-bearing responses.
- **Generated images never auto-deliver.** The model delivers one only by emitting a
  `MEDIA:/absolute/path` token in its final response (resolved against the managed cache first,
  then CWD-confined raw paths); at most 4 logical images per request. Previews ride tool results;
  follow-ups replay text only.
- **Reasoning is never persisted** in conversation or tool history. Summaries and tool history are
  untrusted lower-authority reference — quoted context, never deployment instruction.

## Images and retries

Attached/replied images arrive with `[Image attached at: <private cache path>]` annotations in the
same user message as text and bytes; new turns persist the annotations so follow-ups keep the paths
without replaying bytes. Only the newest 4 loaded images and 4 generated previews stay attached
after each tool batch. Retry
sequences shrink images (down to 0.75 scale, 1920px edge) across 5 attempts for recognized resource
failures; Qwen parser failure retries the identical request once.

## Tests

Turn tests use fake model clients with scripted tool-call sequences. Assert budget fit/over-budget
shapes, blocked-call correlation, retry exhaustion, MEDIA resolution vs denial, and stateless
non-persistence — not prompt text snapshots.
