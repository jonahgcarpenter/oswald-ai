# mcp/ — Lazy Profile-Configured Bridge

Applies on top of the root `AGENTS.md` (trusted-operator-config and never-log-rules).

## Shape

One package, one topic per file. `manager.go` owns lazy per-profile sessions (re-checking
ownership, DNS-gated dial, paginated `ListTools`); `provider.go` is the bridge surface the model
sees; `naming.go` parses `mcp__<server>__<remote>` qualified names (legacy `server.remote` forms
resolve for history attribution only, never for execution); `schema.go` projects remote schemas;
`result.go` flattens results within rune bounds; `profile_configuration.go` / `validate.go` /
`http_transport.go` enforce the server config rules; `stdio_resolve.go` resolves `npx -y <pkg>`
to the installed npm-cache binary (conservative fallback to plain `npx` so cold machines still
install) and names missing stdio binaries per server. `oauth*.go` implement discovery, browser and
device flows, and the token store with cross-process refresh fencing.

## Bridge protocol (what the model actually sees)

The model never sees servers or deferred tools — exactly `tool_search`, `tool_describe`, and
`tool_call`, all connecting lazily at catalog-build and execution time, never at startup:

- `tool_search`: 1–7 keyword queries plus an optional per-query limit (default 5, clamped 1–25).
  Every query word must appear in the tool text; scoring prefers name over server over description
  over params. Empty hits return the available sources plus a retry hint instead of claiming the
  capability is missing. The description carries the live deferred-tool count and embeds a listing
  when it fits 1,500 runes.
- `tool_describe`: 1–10 deduplicated names with schemas, `not_found`, and `errors`. Bridge and
  already-listed names must be called directly — they surface as errors, never schemas.
- `tool_call`: 1–10 `{name, arguments}` entries executed serially in input order, with required-arg
  validation and bridge self-invocation rejected. Successful single-entry calls return the remote
  text directly under its qualified result name; batches return one plain JSON `results` array in
  input order. Read/write remote tools are eligible; a remote literally named `tools` is reserved.

Projection supports compatible top-level object parameters, required flags, bounded descriptions,
and safe string enums — not full nested JSON Schema. Property names must pass the provider
identifier rule; anything else skips the whole tool. Flattened envelopes stay within 16,000 runes
after receipt (a body cap would mislead — framing counts, and truncation preserves framing first);
oversized describe results omit whole schemas with explicit per-tool errors rather than advertising
partial ones. Remote text of ≥32 runes is wrapped in `<untrusted_tool_result>` with the fixed
external-data warning; shorter results stay bare.

## Configuration

Operators declare `mcp_servers.<name>` with exactly one of `url` (HTTP) or `command` (stdio).
Names are 2–40 lowercase ASCII starting with a letter; `soul` is reserved. At most 32 servers, 32
headers, 32 env entries; map values ≤8,192 bytes, no newlines or NULs. stdio takes `command` plus
bounded args/cwd/`env` and forbids transport/headers/auth keys; HTTP defaults to
`streamable_http` (SSE is the opt-in) and may carry static headers plus tool include/exclude
filters (empty include allows all, exclude wins). OAuth exists only on streamable HTTP and owns
the `Authorization` header — a static one alongside is rejected. An automatic restart
reloads configuration;
inherited servers become independent profile-owned connections, with no runtime global
administration and no encryption key.

## OAuth (the only interactive path)

`oswald mcp auth <server>` is the sole interactive flow (browser with loopback callback and
paste-back, or RFC 8628 device flow); the service itself only refreshes lazily and otherwise fails
closed into a re-login prompt. Discovery tries operator credentials, then loopback CIMD, then DCR
(the `.cimd-off` marker records a refusal), binding tokens to the discovery issuer and reusing the
cached registration redirect URI across runs. Scope selection has a fixed priority — explicit
`oauth.scopes`, then the live 401/403 challenge scope, then PRM `scopes_supported`, then AS
`scopes_supported`, then omitted — with `offline_access` appended when the grant list warrants it.
Refresh posts under a 30s cross-process fence, adopts a peer-rotated pair instead of spending a
burned grant, and discards dead dynamic registrations (operator clients never auto-heal). Tokens,
clients, and metadata persist as 0600 files under `<profile>/mcp-tokens/` (0700).

The boundary to internalize: endpoints, catalogs, commands, and credentials are trusted operator
configuration — local HTTP endpoints and stdio commands run on operator trust, with no
private-range filtering. This is not an SSRF/RCE boundary.

## Management CLI

`oswald mcp list|add|edit|remove|test` owns server lifecycle; all mutations run through the
mediated write path (staged validation before write, previous bytes kept as last-known-good
beside the file, atomic replace) — there is no generic `config set`; `oswald config` is
read-only (`show|get|validate`). `list` never
dials and never prints endpoints, headers, or secrets. `test` dials once via the same transport
constructors as the runtime bridge, reporting a failed server with its connection reason
instead of an empty ok. `remove` keeps `mcp-tokens/<server>.*` unless `--with-tokens`
is passed. Flag secrets (`--oauth-client-secret`, header values) follow the `env set` posture:
never echoed or logged.

## Tests

Cover bridge shapes and limits against fake servers, projection acceptance vs whole-tool skips,
include/exclude filtering, OAuth discovery/refresh/rotation races, and shutdown fencing of late
cache publication.
