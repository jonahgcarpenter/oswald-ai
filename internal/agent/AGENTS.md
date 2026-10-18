# Agent - Foreground Turn Loop

`internal/agent/` owns the iterative model loop: prompt assembly, tool execution, image handling,
retries, and finalization. Stateless API turns share the path but persist nothing.

## Layout

- `agent.go` — `Process()`: validate principal, load soul/files/history/debt, build system prompt,
  iterate model ↔ tool calls, finalize and persist.
- `context.go` — `AssemblePromptContext()`: required system+current turn first, then tail reserve,
  summary, older turns. Never split an exchange or truncate required file context.
- `tools.go` — catalog merge per request, `executeTool` (registry first, else MCP bridge).
- `tool_history.go` — model-visible calls vs persisted redacted/bounded calls.
- `compaction.go` — foreground compaction state, delivered-debt loading.
- `session_images.go`, `image_tool_results.go`, `image_retry.go` — attached/generated image
  planning, newest-4 retention, 5-attempt retry with downscaling.
- `media_tokens.go` — `MEDIA:/absolute/path` resolution against cache then CWD.
- `stream.go` — `StreamChunk` (`thinking`/`content`/`status`/`tool_call`/`tool_result`).
- `session_store.go` — persistence contract (resolve/bind/append/page).

## Rules

- Budget (see `internal/session/AGENTS.md` for estimator): 32,768 context, 256-token safety margin,
  70% compaction trigger. Recent-tail reserve is 25% of input, clamped 2,000–8,000. Required
  oversize context is retained and reported; stateless turns return `over_budget`, never truncate.
- Tool authorization is per-iteration: discovery never authorizes another call in the same batch.
  Execute serially, append a correlated result for every declared call including blocked ones.
  Global caps: 50 handler executions, 30 tool-bearing responses; final completion runs tools-disabled.
- Generated images never auto-deliver. The model delivers one only by emitting its `MEDIA` token;
  at most 4 logical images per request. Cancellation never publishes a completed turn.
- Reasoning is never persisted in conversation or tool history.

## Testing

Use fake model clients with scripted tool-call sequences. Cover budget fit/over-budget, blocked-call
correlation, retry exhaustion, MEDIA resolution/denial, and stateless non-persistence.
