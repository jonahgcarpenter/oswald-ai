# MCP - Profile-Configured Bridge

`internal/mcp/` owns the lazy profile-configured bridge. The model sees exactly `tool_search`,
`tool_describe`, and `tool_call`; deferred tools are named `mcp__<server>__<remote>` and execute
only through `tool_call`.

## Packages

- `mcp` (single package, one topic per file) — lazy per-profile sessions and catalog builds
  (`manager.go`, `provider.go` bridge surface); qualified-name parsing with legacy dotted names
  for history attribution only (`naming.go`); top-level-object schema projection (`schema.go`);
  result flattening within rune bounds (`result.go`); server configuration and transport rules
  (`profile_configuration.go`, `validate.go`, `http_transport.go`); OAuth discovery, browser and
  device flows, token store with cross-process refresh fencing (`oauth*.go`).

## Rules

- Sessions connect lazily during search/describe/call, never at startup. No per-server discovery
  tools, no direct deferred calls, no recent-turn pre-exposure. Legacy dot-qualified names never
  resolve.
- `tool_search`: 1–7 keyword queries, per-query limit 1–25 (default 5); every word must match.
  Empty hits return sources plus a retry hint. `tool_describe`: 1–10 deduplicated names with
  `not_found`/`errors`. `tool_call`: 1–10 entries, serial in-order execution, required-arg
  validation, bridge self-invocation rejected.
- Projection supports compatible top-level object parameters, required flags, bounded descriptions,
  and safe string enums — not full nested JSON Schema. Flattened envelopes stay within 16,000
  runes; oversized describe results omit whole schemas with explicit errors.
- OAuth owns the `Authorization` header; a static one alongside is rejected. OAuth is refused for
  SSE/stdio. `oswald mcp auth <server>` (browser loopback or RFC 8628 device flow) is the only
  interactive path; the service refreshes lazily and fails closed into re-login.
- Tokens persist as 0600 files under `<profile>/mcp-tokens/` (0700). Server names are 2–40
  lowercase ASCII starting with a letter; `soul` reserved. At most 32 servers/headers/env entries.
  Restart reloads configuration; inherited servers become independent profile-owned connections.
  Endpoints, catalogs, commands, and credentials are trusted operator configuration, not an
  SSRF/RCE boundary.

## Testing

Cover bridge shapes and limits, projection acceptance/rejection, include/exclude filtering, OAuth
discovery/refresh/rotation with local handlers, and shutdown fencing of late publication.
