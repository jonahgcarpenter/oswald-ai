# AGENTS.md — Oswald AI Developer Reference

This document contains technical guidance for developers working on Oswald AI.

---

## Project Overview

**Oswald AI** is a pure Go application — an LLM-powered chat agent with a two-stage pipeline that processes queries through web search (optional) and an uncensored response model. It has no JavaScript, TypeScript, or frontend code.

**Architecture layers:**

1. **Gateway** — Discord (raw WebSocket) and local WebSocket (`internal/gateway/`)
2. **Agent/Orchestration** — two-stage pipeline: query generation (search) + response generation (`internal/agent/`)
3. **Search** — SearXNG integration for web search (`internal/search/`)
4. **LLM Provider** — interface-based abstraction; Ollama is the only implementation (`internal/provider/`)

---

## Build / Run / Dev Commands

```bash
# Build binary
go build -o ./tmp/main ./cmd/agent/main.go

# Run directly
go run ./cmd/agent/main.go

# Dev mode with hot-reload (requires `air` installed)
air

# Format all Go code
gofmt -w .

# Vet (static analysis)
go vet ./...

# Tidy module dependencies
go mod tidy

# Build Docker image
docker build -t oswald-ai .
```

The `air` tool is configured via `.air.toml`. It watches `.go`, `.tpl`, `.tmpl`, `.html` files, builds to `./tmp/main`, and cleans up on exit.

---

## Testing

There are **no `*_test.go` files** and `go test ./...` finds nothing. Instead, integration tests are standalone `package main` programs in `test/`:

```bash
# Start the server first (required for all tests)
go run ./cmd/agent/main.go

# Run pipeline test (in a separate terminal)
go run ./test/triage.go

# Run streaming TTFT benchmark (in a separate terminal)
go run ./test/ttft.go
```

Both tests connect via WebSocket to `ws://localhost:8080/ws` and require the Ollama backend to be reachable. They cannot be run simultaneously from the same `test/` directory — invoke each file individually.

| File             | Purpose                                                                                   |
| ---------------- | ----------------------------------------------------------------------------------------- |
| `test/triage.go` | Validates pipeline correctness across 10 prompts; ensures all receive non-empty responses |
| `test/ttft.go`   | Benchmarks Time To First Token for SHORT/MEDIUM/LONG prompts with streaming               |

When adding new integration tests, follow the same `package main` standalone pattern in `test/`.

---

## Code Architecture

### Two-Stage Pipeline

The agent processes every request through two sequential stages:

#### Stage 1: Query Generator (`runQueryGenerator`)

- **Model**: `llama3.2:1` (small, fast)
- **Timeout**: 60 seconds
- **Function**: Decides if web search is needed; gathers raw results if needed
- **Input**: User prompt
- **Output**: `[]search.SearchResult` (or empty if no search)
- **Process**:
  1. Query model receives the prompt + `web_search` tool definition
  2. Runs agentic loop (max 5 iterations)
  3. On each iteration: if no tool calls → done; if tool calls → execute search, append results to accumulator
  4. Cap at 5 total results to protect context window
  5. Return accumulated results

#### Stage 2: Response Generation (`Process`)

- **Model**: `llama2-uncensored:7b` (large, uncensored)
- **Timeout**: 3 minutes
- **Function**: Generates the final user-facing response
- **Input**: System prompt (Oswald) + User prompt (with optional search context)
- **Output**: `AgentResponse` with content and metrics
- **Process**:
  1. Build user prompt: if search results exist, wrap in `<task_briefing>` XML; else use raw prompt
  2. Send to uncensored model with system prompt
  3. Stream response chunks back to callback
  4. Return final response

### Response Prompt Assembly

When search results are present, the user message becomes:

```
<task_briefing>
  <user_question>{original user prompt}</user_question>
<intel>
  <source>Web Search</source>
  <summary>My minions have conducted a search...</summary>
  <content>
Title: {result 1 title}
Content: {result 1 snippet}

Title: {result 2 title}
Content: {result 2 snippet}
...
</content>
</intel>
</task_briefing>

<mission>
Answer the user's question directly, concisely, and in your own voice. Use the provided intel and context to be accurate, but use your personality to be an absolute menace...
</mission>
```

This XML structure tells Oswald to absorb the intel and respond in his own voice, not regurgitate facts.

---

## Key Files & Responsibilities

| File                                 | Lines | Purpose                                                                                 |
| ------------------------------------ | ----- | --------------------------------------------------------------------------------------- |
| `cmd/agent/main.go`                  | 102   | Entrypoint: load config, wire providers, gateways, agent                                |
| `internal/agent/agent.go`            | 305   | Core pipeline: `runQueryGenerator()` + `Process()`                                      |
| `internal/agent/workers.go`          | 59    | Load `config/workers.yaml` and resolve worker configs                                   |
| `internal/gateway/discord.go`        | 482   | Discord WebSocket: auth, heartbeat, message handling, mention resolution, reply context |
| `internal/gateway/websocket.go`      | 103   | Local WS: HTTP upgrade, message streaming                                               |
| `internal/gateway/gateway.go`        | 21    | Gateway interface definition                                                            |
| `internal/search/searxng/client.go`  | 90    | SearXNG HTTP client, search result formatting                                           |
| `internal/provider/ollama/client.go` | 335   | Ollama HTTP client, tool calling, streaming                                             |
| `config/workers.yaml`                | 38    | Model definitions + system prompts                                                      |

---

## Code Style Guidelines

### Formatting

- Use `gofmt` — all code must be `gofmt`-clean. No exceptions.
- There is no `.golangci.yml` or other linter config; `go vet ./...` is the only automated static check.
- Tabs for indentation (enforced by `gofmt`).

### Import Organization

Group imports in three blocks separated by blank lines: stdlib → third-party → internal:

```go
import (
    "bufio"
    "context"
    "encoding/json"
    "fmt"
    "net/http"

    "github.com/gorilla/websocket"

    "github.com/jonahgcarpenter/oswald-ai/internal/agent"
)
```

Never use dot imports or blank-aliased imports unless absolutely necessary.

### Naming Conventions

| Element                      | Convention                                          | Example                                                        |
| ---------------------------- | --------------------------------------------------- | -------------------------------------------------------------- |
| Packages                     | short, lowercase, no underscores                    | `agent`, `gateway`, `ollama`                                   |
| Exported types/structs       | `PascalCase`                                        | `AgentResponse`, `SearchResult`                                |
| Unexported types             | `camelCase`                                         | `messageCreate`, `chatRequest`                                 |
| Interfaces                   | `PascalCase`, semantic names                        | `Provider`, `Service`, `Searcher`                              |
| Exported functions/methods   | `PascalCase`                                        | `NewAgent`, `Process`, `Chat`                                  |
| Unexported functions/methods | `camelCase`                                         | `runQueryGenerator`, `resolveMentions`, `splitMessage`         |
| Receiver variables           | short abbreviation of type                          | `a` for `Agent`, `dg` for `DiscordGateway`, `s` for `Searcher` |
| Constants                    | `PascalCase` if exported, `camelCase` if unexported | `webSearchToolName`, `maxIntelResults`                         |

Constructor functions must be named `NewX` and return a pointer to the constructed type:

```go
func NewAgent(provider provider.Provider, searcher search.Searcher, ...) *Agent { ... }
```

### Error Handling

Use the standard Go idiom — check `err != nil` immediately and wrap with `%w` for context:

```go
conn, _, err := websocket.DefaultDialer.Dial(gatewayURL, nil)
if err != nil {
    return fmt.Errorf("failed to dial Discord Gateway: %w", err)
}
```

- Fatal startup errors → `log.Fatal` or `log.Fatalf` (main.go only)
- Recoverable runtime errors → `log.Printf` with context
- Fire-and-forget paths (e.g., heartbeat JSON marshalling) may swallow errors intentionally — add a `// intentionally ignored` comment when doing so
- Never use `panic` for expected error paths

### Types and Structs

- All structs that cross JSON boundaries must have `json:"field_name"` tags. Use `omitempty` for optional fields.
- Use `json.RawMessage` for deferred/lazy decoding (e.g., Discord gateway payloads).
- Prefer interface-based design for extensibility. The core interfaces are `provider.Provider` and `search.Searcher` — new implementations must satisfy these.
- Propagate `context.Context` through all LLM calls via `context.WithTimeout`.
- No generics — the codebase does not use Go generics; avoid introducing them unless clearly necessary.

### Concurrency

- Launch goroutines for long-running background tasks (heartbeat, typing indicator, gateway loops).
- Always pass loop variables explicitly into goroutine closures to avoid capture bugs:
  ```go
  go func(gw gateway.Service) {
      gw.Start(agentEngine)
  }(gateway)
  ```
- Use `chan struct{}` + `defer close(ch)` for goroutine lifecycle/cancellation signals.
- Use `chan os.Signal` for graceful shutdown in `main.go`.

### Comments

Go doc comments follow conventions: exported APIs get `// FunctionName describes...` doc comments; comments explain intent and business logic, not syntax. Avoid stating the obvious; assume the reader is a capable developer.

#### Doc Comments (Exported APIs & Important Unexported Functions)

**Exported functions and types must have a doc comment.** Pattern:

```go
// NewAgent initializes the Agent with a provider and searcher, returning a ready-to-use instance.
// The query worker drives the agentic search loop; the response worker generates final replies.
func NewAgent(provider provider.Provider, searcher search.Searcher, ...) *Agent

// AgentResponse is the final payload returned to the gateway after processing.
type AgentResponse struct {
    Model    string        // Name of the model that generated the response
    Response string        // The actual response content
    Metrics  *ModelMetrics // Performance metrics from the response model
}
```

**Unexported functions:** Comment if they are:

- Called from other packages (part of internal API contract)
- Implement complex logic (>20 lines)
- Have non-obvious parameters, return values, or side effects
- Launch goroutines or manage resource lifecycle

Example patterns:

```go
// connectAndListen manages a single Discord gateway session, handling authentication,
// heartbeat loops, and message routing. Returns when the connection drops.
func (dg *DiscordGateway) connectAndListen() error

// runQueryGenerator runs the agentic query generator loop with optional web search.
// Returns accumulated raw search results (capped at 5) or empty if no search was needed.
func (a *Agent) runQueryGenerator(ctx context.Context, userPrompt string) []search.SearchResult

// resolveMentions replaces every <@ID> and <@!ID> token with @username using the mentions
// list Discord embeds in the MESSAGE_CREATE payload.
func resolveMentions(text string, mentions []struct{ID, Username string}) string
```

Skip comments for trivial helpers: `truncate()`, `splitMessage()` — their names are self-explanatory.

#### Section Comments for Long Functions

Use section comments in functions >75 lines or with 3+ logical phases. Pattern: `// [Action] [Brief Context]`

```go
// Accumulate raw search results across iterations
// Cap at maxIntelResults to protect context window
// Stop loop once results are sufficient
```

Examples from codebase:

- `connectAndListen()` in discord.go: Dial → Heartbeat → Identify → Listen → Cleanup
- `Process()` in agent.go: Query Generation → Prompt Assembly → Response Generation → Metrics
- `Chat()` in ollama/client.go: Build Request → Stream → Handle Metrics → Marshal Response

#### Inline Explanation Comments

Reserve inline comments for explaining **why**, not restating **what** the code does:

```go
// Discord includes the mentions data for free in the MESSAGE_CREATE payload;
// no extra API call needed. Resolve IDs to usernames for readability.
prompt = resolveMentions(prompt, msg.Mentions)

// Cap at 5 results to protect the uncensored model's context window from bloat.
// More results = more tokens = slower generation.
if len(allResults) >= maxIntelResults {
    return allResults
}

// NOTE: Discord rate-limits to ~120 requests/min per bot. Typing indicator is
// fire-and-forget; errors are intentionally ignored to avoid blocking the response path.
_ = dg.sendTyping(msg.ChannelID) // intentionally ignored
```

Avoid comments like `i++  // increment i` or `if err != nil {  // check for errors` — the code speaks for itself.

#### Developer Annotations

Use markers consistently to flag non-obvious behavior, improvements, and known issues:

- **NOTE:** Explains a constraint, assumption, or non-obvious behavior that may surprise future readers.
- **TODO:** Marks incomplete implementations, planned improvements, or deferred work.
- **FIX:** Indicates a known bug, workaround, or limitation that should be addressed.

Examples:

```go
// NOTE: Query generator strips the query model's text output and only uses the raw
// SearchResult objects. This forces the small model to be a tool-caller, not a summarizer.

// TODO: Implement exponential backoff for Discord reconnection.
// Currently retries every 5 seconds; on sustained outages, this may trigger rate limits.

// FIX: When a Discord message exceeds 2000 chars, Discord silently truncates instead
// of returning an error. We detect and split manually, but this is a workaround.
```

---

## Project-Specific Patterns

### Adding a New Search Provider

1. Create a new package under `internal/search/` (e.g., `internal/search/bing/`).
2. Implement the `search.Searcher` interface defined in `internal/search/search.go`.
3. Return `[]search.SearchResult` with `ID`, `Title`, `URL`, `Content` fields populated.
4. Wire into `cmd/agent/main.go` in the `main()` function.

### Adding a New LLM Provider

1. Create a new package under `internal/provider/` (e.g., `internal/provider/openai/`).
2. Implement the `provider.Provider` interface defined in `internal/provider/provider.go`.
3. Define provider-specific JSON structs in a `types.go` file.
4. Wire into `cmd/agent/main.go` in the `main()` function.

### Adding a New Gateway

1. Create a new file under `internal/gateway/` (e.g., `slack.go`).
2. Implement the `gateway.Service` interface defined in `internal/gateway/gateway.go`.
3. Register in `cmd/agent/main.go` alongside existing gateways.
4. Document in README.md with usage examples.

### Query Generator Customization

The query model's behavior is driven entirely by its system prompt in `config/workers.yaml` (QUERY worker). To modify:

- **Guardrails**: Add/remove content categories under "Content Guardrails — Respond with an empty message immediately"
- **Query construction rules**: Add/remove rules under "Query Construction Rules"
- **Examples**: Add few-shot examples showing bad → good queries

The `runQueryGenerator()` function itself doesn't need modification for prompt changes.

### Response Customization

The uncensored model's personality is defined entirely by its system prompt in `config/workers.yaml` (GENERAL worker). Modify the "Commandments" section to change Oswald's behavior, tone, or response style.

### Web Search Configuration

Hardcoded limits:

- **maxIntelResults** = 5 (results accumulated across all search calls)
- **timeout** = 60 seconds (query generator stage)

To change:

- Edit `maxIntelResults` constant in `internal/agent/agent.go`
- Edit timeout in `Process()` method (`60*time.Second`)

---

## Debugging Tips

### Enable Debug Logging

```bash
LOG_LEVEL=DEBUG go run ./cmd/agent/main.go
```

This logs:

- Full prompt inspection (system + user)
- Each search iteration and query
- Result accumulation
- Mention resolution details
- Reply context detection

### Inspect Message Payloads

Add temporary debug logging in `handleMessage()` (discord.go) to print:

- Raw message content before cleaning
- Mention tokens found
- Reply context detected
- Final prompt assembled

### Test Query Generator Isolation

Bypass the response stage and test the query generator directly:

```go
// In a test file or temporary main
agent := NewAgent(provider, searcher, ...)
results := agent.runQueryGenerator(ctx, "your test prompt")
fmt.Printf("Results: %+v\n", results)
```

### Monitor Token Usage

With `LOG_LEVEL=DEBUG`, model metrics are logged after each LLM call:

- `EvalCount` = tokens generated
- `EvalDuration` = time to generate (nanoseconds)
- `TokensPerSecond` = throughput

Use this to identify slow models or context window saturation.

---

## Environment Configuration

All config is loaded from environment variables (with `.env` support via `godotenv`):

| Variable         | Default                  | Purpose                                   |
| ---------------- | ------------------------ | ----------------------------------------- |
| `PORT`           | `8080`                   | HTTP server port                          |
| `OLLAMA_URL`     | `http://localhost:11434` | Ollama API                                |
| `SEARXNG_URL`    | `http://localhost:8888`  | SearXNG API                               |
| `WORKERS_CONFIG` | `config/workers.yaml`    | Worker definitions                        |
| `DISCORD_TOKEN`  | `` (empty)               | Discord bot token                         |
| `LOG_LEVEL`      | `info`                   | Logging: `debug`, `info`, `warn`, `error` |

Implementation: `internal/config/config.go` uses `godotenv.Load()` + `os.Getenv()` pattern.

---

## Dependencies

| Package                        | Version  | Purpose                                             |
| ------------------------------ | -------- | --------------------------------------------------- |
| `github.com/gorilla/websocket` | v1.5.3   | WebSocket client (Discord) and server (local WS)    |
| `github.com/joho/godotenv`     | v1.5.1   | `.env` file loading                                 |
| Go stdlib                      | go 1.25+ | `net/http`, `encoding/json`, `context`, `log`, etc. |

No web framework is used — the HTTP server is raw `net/http`.

After adding or removing dependencies, always run `go mod tidy`.

---

## Performance Considerations

### Context Window Protection

- Query generator caps results at 5 to prevent context bloat
- Raw results injected as-is (no summarization = more tokens)
- Uncensored model has ~7B parameters; 5 results should fit comfortably in a 4K context window

### Streaming Optimization

- Query generation is non-streaming (60-second budget, no output)
- Response generation is streaming (callbacks fire immediately)
- Typing indicators in Discord refresh every 9 seconds while generating

### Timeout Tuning

- Query generator: 60 seconds (needs time for web search + API calls)
- Response generator: 3 minutes (large model, streaming)
- Adjust via context timeout in `Process()` method

### Memory Usage

- Standalone binary, no framework overhead
- Discord heartbeat loop in background (~1 goroutine per bot)
- Typing indicator loop per message (~1 goroutine per request, short-lived)
- No persistent message history in memory

---

## Roadmap & Known Limitations

### Planned Features

- [ ] Multi-turn conversation history per Discord user
- [ ] Rate limiting (per-user, per-guild)
- [ ] Custom guardrails configuration (YAML-driven, not hardcoded)
- [ ] Gateway routing (send certain queries to different gateways)
- [ ] Tool calling in the response stage (not just query stage)

### Known Limitations

- No session resumption: Discord reconnections restart fresh (no `session_id` caching)
- No message persistence: each request is stateless
- Single search provider: only SearXNG is implemented
- Single LLM provider: only Ollama is implemented
- No authentication: WebSocket API is open to local network

---

**Oswald AI** — A developer-friendly, uncensored chat agent in pure Go.
