# Agent - Foreground Turn Loop

`internal/agent/` owns the iterative model loop: prompt assembly, tool execution, image handling,
retries, and finalization. Stateless API turns share the path but persist nothing.

## Packages

- `agent` (single package, one topic per file) — `Process()` loop (`agent.go`), request/response
  contracts (`types.go`), prompt assembly (`context.go`), catalog merge and execution
  (`tools.go`), model-visible vs persisted tool history (`tool_history.go`), foreground
  compaction state (`compaction.go`), session image planning and retention
  (`session_images.go`, `image_tool_results.go`, `image_retry.go`), `MEDIA` token resolution
  (`media_tokens.go`), streaming chunks (`stream.go`), persistence contract
  (`session_store.go`).

## Rules

- Budget (estimator in `session/compaction/budget/`): 32,768 context, 256-token safety margin,
  70% compaction trigger. Recent-tail reserve is 25% of input, clamped 2,000–8,000. Required
  oversize context is retained and reported; stateless turns return `over_budget`, never truncate.
- Tool authorization is per-iteration: discovery never authorizes another call in the same batch.
  Execute serially and append a correlated result for every declared call, including blocked ones.
  Caps: 50 handler executions, 30 tool-bearing responses; the final completion runs tools-disabled.
- Generated images never auto-deliver. The model delivers one only by emitting its `MEDIA` token;
  at most 4 logical images per request. Cancellation never publishes a completed turn.
- Reasoning is never persisted in conversation or tool history.

## Testing

Use fake model clients with scripted tool-call sequences. Cover budget fit/over-budget, blocked-call
correlation, retry exhaustion, MEDIA resolution/denial, and stateless non-persistence.
