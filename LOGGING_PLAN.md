# Logging Plan

## Goals

- Define a uniform JSON log structure for Loki and Grafana.
- Cleanly distinguish server logs from agent logs.
- Establish a shared foundation for all agent logs.
- Let each agent step add only the extra fields it needs.
- Keep the schema stable enough for dashboards, filtering, and alerting.

## Top-Level Split

The application should treat logs as two primary families:

1. Server logs
2. Agent logs

This split should be explicit in the log payload rather than inferred from freeform messages.

Recommended top-level field:

```json
{
  "log_type": "server"
}
```

or:

```json
{
  "log_type": "agent"
}
```

## Shared JSON Envelope

Every log line should use the same base envelope.

```json
{
  "ts": "2026-04-26T21:14:03.821Z",
  "level": "info",
  "service": "oswald-ai",
  "log_type": "server",
  "component": "gateway.discord",
  "event": "gateway.request.received",
  "msg": "received request"
}
```

Core fields:

- `ts`: RFC3339Nano UTC timestamp.
- `level`: `debug`, `info`, `warn`, or `error`.
- `service`: fixed service name, `oswald-ai`.
- `log_type`: `server` or `agent`.
- `component`: stable subsystem name.
- `event`: stable machine-oriented event name.
- `msg`: short human-readable summary.

These fields should be present on every log line regardless of source.

## Golden Schema

This is the canonical production shape the rest of the document should follow.

### Universal Envelope

Required on every log line:

- `ts`
- `level`
- `service`
- `log_type`
- `component`
- `event`
- `msg`

Canonical example:

```json
{
  "ts": "2026-04-26T21:14:03.821Z",
  "level": "info",
  "service": "oswald-ai",
  "log_type": "server",
  "component": "gateway.discord",
  "event": "gateway.request.received",
  "msg": "received request"
}
```

### Request-Scoped Extension

Required on every request-scoped log:

- `request_id`

Common optional fields for request-scoped server logs:

- `gateway`
- `chat_id`
- `session_id`
- `user_id`
- `status`
- `duration_ms`
- `error`

### Agent Foundation

Required on every agent log:

- `request_id`
- `session_id`
- `user_id`
- `gateway`
- `model`

Canonical example:

```json
{
  "ts": "2026-04-26T21:14:03.821Z",
  "level": "debug",
  "service": "oswald-ai",
  "log_type": "agent",
  "component": "agent",
  "event": "agent.request.start",
  "msg": "agent request started",
  "request_id": "req_abc123",
  "session_id": "discord:123:456",
  "user_id": "user_42",
  "gateway": "discord",
  "model": "jaahas/qwen3.5-uncensored:4b"
}
```

### Standard Metric Fields

Prefer these reusable metric fields across events whenever they apply:

- `duration_ms`
- `http_status`
- `status`
- `worker_id`
- `iteration`
- `iteration_count`
- `image_count`
- `tool_count`
- `tool_call_count`
- `gateway_count`
- `turn_count`
- `turn_pair_count`
- `removed_pair_count`
- `historical_message_count`
- `prompt_chars`
- `response_chars`
- `thinking_chars`
- `content_chars`

The rest of the plan should prefer these names rather than inventing event-local variants.

## Server Logs

Server logs are operational runtime logs for the application itself.

Examples:

- startup and shutdown
- gateway listeners
- websocket lifecycle
- discord and iMessage transport activity
- broker queueing and worker activity
- account linking
- tool registry loading
- provider HTTP failures
- memory store maintenance

Recommended server-specific fields when applicable:

- `gateway`
- `chat_id`
- `session_id`
- `user_id`
- `worker_id`
- `status`
- `duration_ms`
- `error`

Server logs should focus on transport, queueing, persistence, external integrations, and runtime health.

## Agent Logs

Agent logs should be treated as a separate structured stream for the request lifecycle inside `Agent.Process()` and related agent execution steps.

Every agent log should inherit a common foundation, then each step should append only the extra details relevant to that step.

### Agent Foundation

Every agent log should include the full shared envelope plus a standard agent foundation.

```json
{
  "ts": "2026-04-26T21:14:03.821Z",
  "level": "debug",
  "service": "oswald-ai",
  "log_type": "agent",
  "component": "agent",
  "event": "agent.request.start",
  "msg": "agent request started",
  "request_id": "req_abc123",
  "session_id": "discord:123:456",
  "user_id": "user_42",
  "gateway": "discord",
  "model": "jaahas/qwen3.5-uncensored:4b"
}
```

Required foundation fields for agent logs:

- `request_id`: generated once per prompt and reused through the entire request lifecycle.
- `session_id`: session memory key for debugging request continuity.
- `user_id`: canonical internal user identifier.
- `gateway`: request source such as `discord`, `websocket`, or `imessage`.
- `model`: active Ollama model for the request.

These fields should be present on every agent log line so Grafana queries can correlate a full request without parsing message text.

### Agent Step Enrichment

Each agent step should add a small, step-specific set of fields.

Examples:

1. Request start

Additional fields:

- `prompt_chars`
- `image_count`

2. Memory load

Additional fields:

- `historical_message_count`
- `turn_pair_count`

3. Context compaction

Additional fields:

- `removed_pair_count`
- `estimated_before`
- `estimated_after`
- `prompt_budget`

4. Model call

Additional fields:

- `iteration`
- `is_streaming`
- `tool_count`

5. Agent loop iteration

Additional fields:

- `iteration`
- `tool_call_count`
- `thinking_chars`
- `content_chars`
- `failure_streak`

6. Tool execution

Additional fields:

- `iteration`
- `tool_name`
- `status`
- `failure_streak`
- `max_failures`

7. Final response

Additional fields:

- `iteration_count`
- `response_chars`
- `thinking_chars`
- `duration_ms`
- `tool_failure_budget_exhausted`

The rule should be:

- all agent logs share the same foundation
- each event adds only the fields needed for that event
- no event should invent a completely different shape

## Event Naming

Use stable dotted event names instead of embedding structure in message text.

Examples for server logs:

- `app.start`
- `app.shutdown`
- `gateway.listen`
- `gateway.request.received`
- `gateway.response.sent`
- `broker.request.queued`
- `broker.request.rejected`
- `broker.worker.started`
- `provider.ollama.chat.http_error`
- `memory.turn.pruned`

Examples for agent logs:

- `agent.request.start`
- `agent.memory.loaded`
- `agent.context.compacted`
- `agent.context.over_budget`
- `agent.model.call`
- `agent.model.error`
- `agent.loop.iteration`
- `agent.loop.complete`
- `agent.tool.start`
- `agent.tool.success`
- `agent.tool.failure`
- `agent.response.complete`

## Severity Policy

Severity should be assigned by operational meaning, not by which file emitted the log.

Rules:

- `debug`: high-volume request lifecycle details, iteration details, attachment handling details, storage details, and other diagnostics mainly used during development or incident investigation
- `info`: startup, shutdown, enabled subsystems, successful one-time initialization, and major operational state transitions
- `warn`: degraded but recoverable behavior, retries, skipped data, parse failures that do not abort the request, and fallback paths
- `error`: failed operations that prevent the intended step from succeeding

Examples:

- `agent.request.start`: `debug`
- `agent.loop.iteration`: `debug`
- `broker.started`: `info`
- `app.start`: `info`
- `gateway.send.retry`: `warn`
- `provider.ollama.chat.stream.parse_failed`: `warn`
- `agent.tool.failure`: `warn`
- `agent.model.error`: `error`
- `gateway.account.resolve_failed`: `error`

## Required Fields Matrix

All logs must include:

- `ts`
- `level`
- `service`
- `log_type`
- `component`
- `event`
- `msg`

All request-scoped logs must include:

- `request_id`

All agent logs must include:

- `request_id`
- `session_id`
- `user_id`
- `gateway`
- `model`

Server logs should include `request_id` only when they are directly tied to a single in-flight prompt.

Examples of request-scoped server logs:

- `gateway.request.received`
- `broker.request.queued`
- `broker.request.rejected`
- `gateway.response.sent`
- `provider.ollama.chat.http_error`

Examples of non-request-scoped server logs:

- `app.start`
- `app.shutdown`
- `broker.started`
- `gateway.listen`
- `tool.bootstrap.enabled`

## Field Naming Conventions

Field names should stay consistent across all files.

Rules:

- identifiers end with `_id`
- counts end with `_count`
- durations end with `_ms`
- character lengths end with `_chars`
- booleans should consistently use `is_` prefixes when they represent state flags
- use singular names for scalar values and plural names only for arrays

Examples:

- `request_id`
- `session_id`
- `user_id`
- `chat_id`
- `gateway_count`
- `image_count`
- `tool_call_count`
- `duration_ms`
- `prompt_chars`
- `response_chars`
- `is_dm`
- `is_reply`
- `is_group`

Avoid inconsistent alternatives such as mixing:

- `tool_calls` and `tool_call_count`
- `turns` and `turn_count`
- `thinking_len` and `thinking_chars`
- `content_len` and `content_chars`

Prefer the more explicit count and size suffixes when the value is numeric.

## Status Vocabulary

When `status` is used, it should come from a small fixed set:

- `ok`
- `error`
- `rejected`
- `retry`
- `degraded`

Guidance:

- use `ok` for successful completion
- use `error` for failed operations
- use `rejected` when work is refused before execution
- use `retry` when a fallback or second attempt is being made
- use `degraded` when the operation continues with reduced quality or missing data

## Redaction And Truncation Policy

The log plan should explicitly constrain logged payload size and sensitive content.

Rules:

- never log full prompt text
- never log full response text
- never log raw image data or base64 payloads
- never log full tool results
- never log full provider response bodies by default
- never log authorization secrets, tokens, passwords, or credentials

Preferred replacements:

- `prompt_chars` instead of prompt body
- `response_chars` instead of response body
- `image_count` instead of image content
- `tool_call_count` instead of tool result blobs
- `http_status` instead of large provider body payloads

Truncation guidance:

- `error` strings should be kept short and bounded
- any provider body included for diagnostics should be explicitly truncated before logging
- display-oriented string fields should remain small enough to be useful in Grafana tables without wrapping into unreadable blobs

## High-Volume Event Policy

Some events are naturally high-volume and should remain `debug` unless there is a strong operational need to raise them.

High-volume examples:

- `agent.loop.iteration`
- `agent.model.call`
- `gateway.attachment.processed`
- `memory.turn.appended`
- `memory.turn.pruned`
- `provider.ollama.chat.stream.parse_failed` when a provider is unstable

Guidance:

- high-frequency lifecycle events should default to `debug`
- only state transitions and significant outcomes should rise to `info`
- repeated noisy warnings should be reconsidered if they become common during normal operation

## Array Usage Guidance

Arrays are allowed, but they should be used sparingly because they are less convenient for dashboards than scalar fields.

Use arrays only when the set is naturally small and low-frequency, such as:

- enabled gateway names at startup
- enabled tool names at startup

Prefer scalar summaries for hot-path logs:

- `gateway_count` instead of repeatedly logging full gateway lists
- `tool_count` instead of large tool arrays on request logs
- `tool_count` instead of large tool arrays on request logs
- `image_count` instead of attachment detail arrays on request logs

## Duration Semantics

Whenever `duration_ms` is logged, its measurement scope should be clear from the event name.

Recommended meanings:

- `agent.response.complete`: total agent request duration
- `agent.tool.success` or `agent.tool.failure`: tool execution duration
- `provider.ollama.chat.http_error` or provider success events if added later: upstream request duration
- `gateway.response.sent`: send operation duration when available

Do not compare `duration_ms` values across unrelated events unless the event names describe the same measurement scope.

## Message Field Guidance

The `msg` field should stay short and human-readable.

Good examples:

- `received request`
- `queued broker request`
- `context compacted`
- `tool execution failed`
- `response completed`

Avoid putting variable data inside `msg` when that data should live in fields.

## Field Placement Rules

Use these rules consistently:

- Put common query keys at top level.
- Put correlation keys at top level.
- Put numeric metrics for dashboards at top level when they are reused often.
- Keep event-specific fields small and predictable.
- Do not bury core operational fields inside a freeform object.

Optional rule if needed later:

- reserve an `attrs` object for rare event-specific overflow fields

The initial implementation should stay as flat as possible.

## Request Correlation

The primary correlation unit should be `request_id`.

Plan:

1. Generate `request_id` at the gateway boundary for each prompt.
2. Store it on `broker.Request`.
3. Pass it into agent processing.
4. Include it in all logs created while handling that request.

This gives clean per-prompt filtering in Loki without relying on session IDs as labels.

## Loki Label Guidance

Recommended labels:

- `service`
- `level`
- `log_type`
- `component`
- `event`

Potential label depending on dashboard value:

- `gateway`

Do not use these as Loki labels because they are high-cardinality:

- `request_id`
- `session_id`
- `user_id`
- `chat_id`
- `tool_name` if tool count grows or usage is bursty

These fields should remain in the JSON payload for filtering at query time.

## Data Hygiene

Do not log these as normal structured fields:

- full prompt text
- full response text
- raw image payloads
- base64 data
- large tool results
- large provider response bodies

Prefer summary fields instead:

- `prompt_chars`
- `response_chars`
- `image_count`
- `tool_call_count`
- `http_status`
- `duration_ms`

## Dashboard Targets

The schema should support a small set of clear operational dashboards from the start.

Suggested dashboards:

1. Request volume by gateway
2. Request latency by gateway and model
3. Broker queue rejection rate
4. Tool execution success and failure counts
5. Ollama provider error rate
6. Context compaction frequency
7. Agent tool-failure budget exhaustion count
8. iMessage and Discord send failure rate

These dashboards justify keeping these fields easy to query:

- `gateway`
- `model`
- `event`
- `level`
- `status`
- `duration_ms`
- `tool_name`
- `http_status`

## Validation Checklist

When implementation begins, the migration should be checked against this list:

1. every emitted log line is valid JSON
2. every log line includes the shared envelope fields
3. every agent log includes the full agent foundation
4. every request-scoped log includes `request_id`
5. no normal log includes full prompt or response bodies
6. no normal log includes raw image payloads or secrets
7. event names come from a stable predefined set
8. numeric dashboard fields use consistent names and suffixes
9. Loki labels remain low-cardinality
10. sample Grafana queries can filter by `request_id`, `gateway`, `event`, and `model`

## Implementation Shape

When this plan is implemented, the logger should support:

1. a shared base JSON envelope for all logs
2. a clear `server` vs `agent` distinction
3. an agent logging helper that automatically injects the agent foundation
4. step-level enrichment by adding extra fields per event

That means the future code shape should favor:

- a general JSON logger for all components
- a request-scoped agent logger or helper derived from that logger
- stable event names instead of formatted strings

## Rollout Order

When implementation begins, convert logs in this order:

1. logger implementation in `internal/config/logging.go`
2. request ID generation and propagation through gateways, broker, and agent
3. agent logs first, using the shared agent foundation
4. broker and gateway server logs
5. provider, memory, tools, and account-link server logs

This keeps the highest-value request tracing work first while preserving a clear split between server and agent telemetry.

## File Event Map

This section maps the current codebase files to their target event examples.

The goal is not to create a unique schema per file. The goal is to show which stable event names each file should emit once the migration is implemented.

### `cmd/agent/main.go`

Log type: `server`

Component:

- `app`

Event examples:

- `app.config.loaded`
- `app.context_budget.resolved`
- `app.trace.enabled`
- `app.start`
- `app.gateway.stopped`
- `app.shutdown`

Example payload shapes:

```json
{
  "service": "oswald-ai",
  "level": "info",
  "log_type": "server",
  "component": "app",
  "event": "app.start",
  "msg": "starting application"
}
```

```json
{
  "service": "oswald-ai",
  "level": "info",
  "log_type": "server",
  "component": "app",
  "event": "app.context_budget.resolved",
  "msg": "resolved context budget",
  "model": "jaahas/qwen3.5-uncensored:4b",
  "context_window": 32768,
  "prompt_budget": 24576,
  "source": "ollama_show"
}
```

### `internal/gateway/bootstrap.go`

Log type: `server`

Component:

- `gateway.bootstrap`

Event examples:

- `gateway.bootstrap.enabled`

Example payload shape:

```json
{
  "service": "oswald-ai",
  "level": "info",
  "log_type": "server",
  "component": "gateway.bootstrap",
  "event": "gateway.bootstrap.enabled",
  "msg": "resolved enabled gateways",
  "gateway_count": 3,
  "gateways": ["websocket", "discord", "imessage"]
}
```

### `internal/broker/broker.go`

Log type: `server`

Component:

- `broker`

Event examples:

- `broker.started`
- `broker.request.queued`
- `broker.request.rejected`
- `broker.worker.started`
- `broker.worker.processing`
- `broker.worker.stopped`
- `broker.shutdown.start`
- `broker.shutdown.complete`

Example payload shapes:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "broker",
  "event": "broker.request.queued",
  "msg": "queued broker request",
  "request_id": "req_abc123",
  "gateway": "discord",
  "chat_id": "12345",
  "session_id": "discord:12345:999"
}
```

```json
{
  "service": "oswald-ai",
  "level": "warn",
  "log_type": "server",
  "component": "broker",
  "event": "broker.request.rejected",
  "msg": "rejected broker request",
  "request_id": "req_abc123",
  "gateway": "discord",
  "chat_id": "12345",
  "status": "rejected",
  "reason": "queue_full"
}
```

### `internal/agent/agent.go`

Log type: `agent`

Component:

- `agent`

This file is the primary source of request-scoped agent logs and should define the strongest version of the shared agent foundation.

Event examples:

- `agent.request.start`
- `agent.soul.read_failed`
- `agent.memory.loaded`
- `agent.context.compacted`
- `agent.context.over_budget`
- `agent.model.call`
- `agent.model.error`
- `agent.loop.iteration`
- `agent.loop.complete`
- `agent.tool.start`
- `agent.tool.success`
- `agent.tool.failure`
- `agent.tool_budget.exhausted`
- `agent.response.complete`
- `agent.trace.write_failed`
- `agent.trace.written`

Example payload shapes:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "agent",
  "component": "agent",
  "event": "agent.request.start",
  "msg": "agent request started",
  "request_id": "req_abc123",
  "session_id": "discord:12345:999",
  "user_id": "user_42",
  "gateway": "discord",
  "model": "jaahas/qwen3.5-uncensored:4b",
  "prompt_chars": 148,
  "image_count": 1
}
```

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "agent",
  "component": "agent",
  "event": "agent.loop.iteration",
  "msg": "completed agent loop iteration",
  "request_id": "req_abc123",
  "session_id": "discord:12345:999",
  "user_id": "user_42",
  "gateway": "discord",
  "model": "jaahas/qwen3.5-uncensored:4b",
  "iteration": 2,
  "tool_call_count": 1,
  "thinking_chars": 320,
  "content_chars": 0,
  "failure_streak": 0
}
```

```json
{
  "service": "oswald-ai",
  "level": "warn",
  "log_type": "agent",
  "component": "agent",
  "event": "agent.tool.failure",
  "msg": "tool execution failed",
  "request_id": "req_abc123",
  "session_id": "discord:12345:999",
  "user_id": "user_42",
  "gateway": "discord",
  "model": "jaahas/qwen3.5-uncensored:4b",
  "iteration": 2,
  "tool_name": "web_search",
  "failure_streak": 1,
  "max_failures": 3,
  "status": "error",
  "error": "timeout contacting searxng"
}
```

### `internal/agent/summarize.go`

Log type: `agent`

Component:

- `agent.summarizer`

Event examples:

- `agent.summarizer.generated`

Example payload shape:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "agent",
  "component": "agent.summarizer",
  "event": "agent.summarizer.generated",
  "msg": "generated history summary",
  "request_id": "req_abc123",
  "session_id": "discord:12345:999",
  "user_id": "user_42",
  "gateway": "discord",
  "model": "jaahas/qwen3.5-uncensored:4b",
  "summary_chars": 640,
  "turn_pair_count": 8
}
```

### `internal/ollama/client.go`

Log type: `server`

Component:

- `provider.ollama`

Event examples:

- `provider.ollama.show.http_error`
- `provider.ollama.show.decode_error`
- `provider.ollama.chat.http_error`
- `provider.ollama.chat.decode_error`
- `provider.ollama.chat.stream.parse_failed`
- `provider.ollama.chat.stream.scan_failed`

Example payload shapes:

```json
{
  "service": "oswald-ai",
  "level": "error",
  "log_type": "server",
  "component": "provider.ollama",
  "event": "provider.ollama.chat.http_error",
  "msg": "ollama chat returned non-2xx",
  "request_id": "req_abc123",
  "model": "jaahas/qwen3.5-uncensored:4b",
  "operation": "chat",
  "http_status": 500,
  "status": "error"
}
```

```json
{
  "service": "oswald-ai",
  "level": "warn",
  "log_type": "server",
  "component": "provider.ollama",
  "event": "provider.ollama.chat.stream.parse_failed",
  "msg": "failed to parse stream chunk",
  "request_id": "req_abc123",
  "model": "jaahas/qwen3.5-uncensored:4b",
  "operation": "chat_stream",
  "status": "degraded",
  "error": "invalid JSON chunk"
}
```

### `internal/memory/store.go`

Log type: `server`

Component:

- `memory.session`

Event examples:

- `memory.turn.appended`
- `memory.turn.pruned`

Example payload shapes:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "memory.session",
  "event": "memory.turn.appended",
  "msg": "appended session turn",
  "session_id": "discord:12345:999",
  "turn_count": 6,
  "operation": "append"
}
```

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "memory.session",
  "event": "memory.turn.pruned",
  "msg": "pruned session turns",
  "session_id": "discord:12345:999",
  "operation": "append",
  "expired_count": 2,
  "overflow_count": 1,
  "retained_turn_count": 6,
  "max_turn_count": 10,
  "max_age": "30m"
}
```

### `internal/gateway/websocket/gateway.go`

Log type: `server`

Component:

- `gateway.websocket`

Event examples:

- `gateway.listen`
- `gateway.connection.upgrade_failed`
- `gateway.connection.closed`
- `gateway.attachment.processed`
- `gateway.account.resolve_failed`
- `gateway.command.failed`
- `gateway.request.received`
- `gateway.stream.started`
- `gateway.stream.marshal_failed`
- `gateway.response.failed`
- `gateway.response.sent`
- `gateway.write_failed`

Example payload shapes:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "gateway.websocket",
  "event": "gateway.request.received",
  "msg": "received websocket request",
  "request_id": "req_abc123",
  "gateway": "websocket",
  "session_id": "websocket:127.0.0.1",
  "user_id": "user_42",
  "prompt_chars": 148,
  "image_count": 1
}
```

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "gateway.websocket",
  "event": "gateway.attachment.processed",
  "msg": "processed websocket attachments",
  "gateway": "websocket",
  "chat_id": "127.0.0.1:54321",
  "accepted_count": 1,
  "downgraded_count": 1,
  "declared_format_count": 2
}
```

### `internal/gateway/discord/gateway.go`

Log type: `server`

Component:

- `gateway.discord`

Event examples:

- `gateway.connection.dropped`
- `gateway.heartbeat.failed`
- `gateway.session.ready`
- `gateway.session.resumed`
- `gateway.attachment.processed`
- `gateway.account.normalize_failed`
- `gateway.account.resolve_failed`
- `gateway.command.failed`
- `gateway.reply_context.applied`
- `gateway.request.received`
- `gateway.typing.started`
- `gateway.response.failed`
- `gateway.response.sent`
- `gateway.send.failed`

Example payload shapes:

```json
{
  "service": "oswald-ai",
  "level": "info",
  "log_type": "server",
  "component": "gateway.discord",
  "event": "gateway.session.ready",
  "msg": "discord gateway ready",
  "gateway": "discord",
  "bot_id": "1234567890",
  "bot_username": "oswald"
}
```

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "gateway.discord",
  "event": "gateway.request.received",
  "msg": "received discord request",
  "request_id": "req_abc123",
  "gateway": "discord",
  "chat_id": "12345",
  "session_id": "discord:12345:999",
  "user_id": "user_42",
  "image_count": 1,
  "is_dm": false,
  "is_reply": true,
  "prompt_chars": 148
}
```

### `internal/gateway/imessage/gateway.go`

Log type: `server`

Component:

- `gateway.imessage`

Event examples:

- `gateway.listen`
- `gateway.webhook.read_failed`
- `gateway.webhook.decode_failed`
- `gateway.webhook.ignored`
- `gateway.attachment.processed`
- `gateway.account.normalize_failed`
- `gateway.contact_lookup.failed`
- `gateway.account.resolve_failed`
- `gateway.reply_context.applied`
- `gateway.typing.start_failed`
- `gateway.request.received`
- `gateway.command.failed`
- `gateway.response.failed`
- `gateway.response.sent`
- `gateway.send.failed`
- `gateway.send.retry`
- `gateway.send.provider_failed`

Example payload shapes:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "gateway.imessage",
  "event": "gateway.request.received",
  "msg": "received imessage request",
  "request_id": "req_abc123",
  "gateway": "imessage",
  "chat_id": "iMessage;-;chat123",
  "session_id": "imessage:dm:+15555555555",
  "user_id": "user_42",
  "image_count": 2,
  "is_group": false,
  "is_reply": true,
  "prompt_chars": 148
}
```

```json
{
  "service": "oswald-ai",
  "level": "warn",
  "log_type": "server",
  "component": "gateway.imessage",
  "event": "gateway.send.retry",
  "msg": "retrying imessage send with fallback method",
  "gateway": "imessage",
  "chat_id": "iMessage;-;chat123",
  "default_method": "send-message",
  "fallback_method": "send-text",
  "status": "retry",
  "error": "provider returned 500"
}
```

### `internal/tools/bootstrap.go`

Log type: `server`

Component:

- `tool.bootstrap`

Event examples:

- `tool.bootstrap.configured`
- `tool.bootstrap.enabled`

Example payload shape:

```json
{
  "service": "oswald-ai",
  "level": "info",
  "log_type": "server",
  "component": "tool.bootstrap",
  "event": "tool.bootstrap.enabled",
  "msg": "enabled builtin tools",
  "tool_count": 3,
  "tools": ["persistent_memory", "soul_memory", "web_search"]
}
```

### `internal/tools/registry.go`

Log type: `server`

Component:

- `tool.registry`

Event examples:

- `tool.registry.definition_loaded`
- `tool.registry.definition_read_failed`
- `tool.registry.definition_parse_failed`
- `tool.registry.empty`
- `tool.registry.handler_registered`

Example payload shapes:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "tool.registry",
  "event": "tool.registry.definition_loaded",
  "msg": "loaded tool definition",
  "tool_name": "web_search",
  "file": "web_search.md"
}
```

```json
{
  "service": "oswald-ai",
  "level": "warn",
  "log_type": "server",
  "component": "tool.registry",
  "event": "tool.registry.definition_parse_failed",
  "msg": "failed to parse tool definition",
  "file": "persistent_memory.md",
  "status": "error",
  "error": "missing parameters section"
}
```

### `internal/tools/websearch/client.go`

Log type: `server`

Component:

- `tool.web_search`

Event examples:

- `tool.web_search.request_failed`
- `tool.web_search.response_failed`
- `tool.web_search.results_returned`

Example payload shape:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "tool.web_search",
  "event": "tool.web_search.results_returned",
  "msg": "web search returned results",
  "request_id": "req_abc123",
  "query_chars": 24,
  "result_count": 5
}
```

### `internal/tools/websearch/websearch.go`

Log type: `agent`

Component:

- `agent.tool.web_search`

Event examples:

- `agent.tool.web_search.start`

Example payload shape:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "agent",
  "component": "agent.tool.web_search",
  "event": "agent.tool.web_search.start",
  "msg": "starting web search tool",
  "request_id": "req_abc123",
  "session_id": "discord:12345:999",
  "user_id": "user_42",
  "gateway": "discord",
  "model": "jaahas/qwen3.5-uncensored:4b",
  "tool_name": "web_search",
  "query_chars": 24
}
```

### `internal/tools/usermemory/usermemory.go`

Log type: `agent`

Component:

- `agent.tool.persistent_memory`

Event examples:

- `agent.tool.persistent_memory.remembered`
- `agent.tool.persistent_memory.not_found`
- `agent.tool.persistent_memory.migration.started`
- `agent.tool.persistent_memory.migration.failed`
- `agent.tool.persistent_memory.migration.persist_failed`
- `agent.tool.persistent_memory.migration.completed`
- `agent.tool.persistent_memory.recalled`
- `agent.tool.persistent_memory.forgot`
- `agent.tool.persistent_memory.wiped`
- `agent.tool.persistent_memory.validation_failed`

Example payload shape:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "agent",
  "component": "agent.tool.persistent_memory",
  "event": "agent.tool.persistent_memory.recalled",
  "msg": "recalled persistent memory",
  "request_id": "req_abc123",
  "session_id": "discord:12345:999",
  "user_id": "user_42",
  "gateway": "discord",
  "model": "jaahas/qwen3.5-uncensored:4b",
  "tool_name": "persistent_memory",
  "category": "preferences",
  "content_chars": 412
}
```

### `internal/tools/usermemory/store.go`

Log type: `server`

Component:

- `memory.user`

Event examples:

- `memory.user.synced_speaker_intro`
- `memory.user.stored`
- `memory.user.deleted_entry`
- `memory.user.deleted_all`
- `memory.user.file_written`
- `memory.user.merged`

Example payload shape:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "memory.user",
  "event": "memory.user.stored",
  "msg": "stored user memory",
  "user_id": "user_42",
  "category": "preferences"
}
```

### `internal/tools/soulmemory/soulmemory.go`

Log type: `agent`

Component:

- `agent.tool.soul_memory`

Event examples:

- `agent.tool.soul_memory.overwritten`
- `agent.tool.soul_memory.appended`

Example payload shape:

```json
{
  "service": "oswald-ai",
  "level": "warn",
  "log_type": "agent",
  "component": "agent.tool.soul_memory",
  "event": "agent.tool.soul_memory.overwritten",
  "msg": "overwrote soul memory",
  "request_id": "req_abc123",
  "session_id": "discord:12345:999",
  "user_id": "user_42",
  "gateway": "discord",
  "model": "jaahas/qwen3.5-uncensored:4b",
  "tool_name": "soul_memory",
  "action": "write"
}
```

### `internal/tools/soulmemory/store.go`

Log type: `server`

Component:

- `memory.soul`

Event examples:

- `memory.soul.written`
- `memory.soul.appended`

Example payload shape:

```json
{
  "service": "oswald-ai",
  "level": "debug",
  "log_type": "server",
  "component": "memory.soul",
  "event": "memory.soul.written",
  "msg": "wrote soul file",
  "path": "config/soul.md",
  "content_chars": 1024
}
```

### `internal/accountlink/store.go`

Log type: `server`

Component:

- `account_link`

Event examples:

- `account_link.canonical_user.created`
- `account_link.users.merged`
- `account_link.account.linked`
- `account_link.account.disconnected`
- `account_link.store.backup_failed`
- `account_link.store.recovered`
- `account_link.store.repaired`

Example payload shape:

```json
{
  "service": "oswald-ai",
  "level": "info",
  "log_type": "server",
  "component": "account_link",
  "event": "account_link.users.merged",
  "msg": "merged linked users",
  "source_user_id": "user_old",
  "target_user_id": "user_42",
  "account": "discord:123456"
}
```

## Notes On File Classification

Some files are technically tool-related but should still emit `agent` logs when they describe request-scoped tool behavior.

Use this rule:

- if the log describes runtime infrastructure, persistence, provider IO, or transport behavior, it is a `server` log
- if the log describes a single in-flight prompt being processed by the agent, it is an `agent` log

Examples:

- `internal/tools/websearch/client.go` is `server` because it describes provider interaction details
- `internal/tools/websearch/websearch.go` is `agent` because it describes a request-scoped tool invocation
- `internal/tools/usermemory/store.go` is `server` because it describes storage behavior
- `internal/tools/usermemory/usermemory.go` is `agent` because it describes tool behavior during a prompt
