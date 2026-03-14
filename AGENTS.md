# AGENTS.md — Oswald AI Coding Agent Reference

This file contains guidance for agentic coding assistants operating in this repository.

---

## Project Overview

Oswald AI is a pure Go application — an LLM-powered chat agent with a triage/routing layer that selects expert models based on query type. It has no JavaScript, TypeScript, or frontend code.

**Architecture layers:**
1. **Gateway** — Discord (raw WebSocket) and local WebSocket (`internal/gateway/`)
2. **Agent/Orchestration** — triage routing, model selection, response streaming (`internal/agent/`)
3. **LLM Provider** — interface-based abstraction; Ollama is the only implementation (`internal/llm/`)

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

# Run triage/routing accuracy test (in a separate terminal)
go run ./test/triage.go

# Run streaming TTFT benchmark (in a separate terminal)
go run ./test/ttft.go
```

Both tests connect via WebSocket to `ws://localhost:8080/ws` and require the Ollama backend to be reachable. They cannot be run simultaneously from the same `test/` directory — invoke each file individually.

| File | Purpose |
|---|---|
| `test/triage.go` | Validates routing accuracy across 20 prompts in 4 categories |
| `test/ttft.go` | Benchmarks Time To First Token for SHORT/MEDIUM/LONG prompts |

When adding new integration tests, follow the same `package main` standalone pattern in `test/`.

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

    "github.com/jonahgcarpenter/oswald-ai/internal/llm"
)
```

Never use dot imports or blank-aliased imports unless absolutely necessary.

### Naming Conventions

| Element | Convention | Example |
|---|---|---|
| Packages | short, lowercase, no underscores | `agent`, `gateway`, `ollama` |
| Exported types/structs | `PascalCase` | `AgentResponse`, `RouteDecision` |
| Unexported types | `camelCase` | `heartbeatPayload` |
| Interfaces | `PascalCase`, semantic names | `Provider`, `Service` |
| Exported functions/methods | `PascalCase` | `NewAgent`, `Generate` |
| Unexported functions/methods | `camelCase` | `heartbeatLoop`, `getEnv` |
| Receiver variables | short abbreviation of type | `a` for `Agent`, `dg` for `DiscordGateway` |
| Constants | `PascalCase` if exported, `camelCase` if unexported | `OswaldBasePrompt`, `gatewayURL` |

Constructor functions must be named `NewX` and return a pointer to the constructed type:

```go
func NewAgent(provider llm.Provider, cfg *config.Config) *Agent { ... }
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
- Prefer interface-based design for extensibility. The two core interfaces are `llm.Provider` and `gateway.Service` — new implementations must satisfy these interfaces.
- Propagate `context.Context` through all LLM calls via `context.WithTimeout`.
- No generics — the codebase does not use Go generics; avoid introducing them unless clearly necessary.

### Concurrency

- Launch goroutines for long-running background tasks (heartbeat, typing indicator, gateway loops).
- Always pass loop variables explicitly into goroutine closures to avoid capture bugs:
  ```go
  go func(g gateway.Service) {
      g.Start(agentEngine)
  }(gw)
  ```
- Use `chan struct{}` + `defer close(ch)` for goroutine lifecycle/cancellation signals.
- Use `chan os.Signal` for graceful shutdown in `main.go`.

### Comments

Go doc comments follow conventions: exported APIs get `// FunctionName describes...` doc comments; comments explain intent and business logic, not syntax. Avoid stating the obvious; assume the reader is a capable developer.

#### Doc Comments (Exported APIs & Important Unexported Functions)

**Exported functions and types must have a doc comment.** Pattern:

```go
// NewAgent initializes the Agent with a provider and logger, returning a ready-to-use instance.
// Pass nil for logger to use the default silent logger.
func NewAgent(provider llm.Provider, log *config.Logger) *Agent

// AgentResponse represents a single message in the agent's response stream.
type AgentResponse struct {
    Content string    // Streamed message content
    IsFinal bool      // True when stream ends
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
// heartbeat loops, and message routing. Returns on connection drop or Discord disconnect signal.
func (dg *DiscordGateway) connectAndListen() error

// getEnv retrieves an environment variable with fallback to the default value.
func getEnv(key, defaultValue string) string

// splitMessage truncates a message to maxLen runes, breaking at newlines/sentence boundaries
// when possible to improve readability. Returns original if already short enough.
func splitMessage(msg string, maxLen int) string
```

Skip comments for trivial helpers: `min()`, `contains()`, `abs()` — their names are self-explanatory.

#### Section Comments for Long Functions

Use section comments in functions >75 lines or with 3+ logical phases. Pattern: `// [Action] [Brief Context]`

```go
// Initialize connection parameters
// Start heartbeat goroutine
// Send identify payload
// Begin message loop
// Cleanup on disconnect
```

Examples from codebase:
- `connectAndListen()` in discord.go: Initialize → Heartbeat → Identify → Listen → Cleanup
- `Process()` in agent.go: Triage → Generate → Adapt → Execute
- `Chat()` in ollama/client.go: Build Request → Stream → Handle Thinking → Marshal Response

#### Inline Explanation Comments

Reserve inline comments for explaining **why**, not restating **what** the code does:

```go
// Thinking models emit streaming content in the Thinking field, leaving Response empty.
// Aggregate into Response to match caller expectations.
callback(resp.Response)

// Fire callback immediately to avoid buffering entire response in memory.
// Streaming consumers will handle chunks as they arrive.
streamCallback(chunk)

// NOTE: Discord rate-limits to ~120 requests/min per bot. Cache user data locally
// to avoid redundant API calls during high-volume message processing.
userCache[id] = user
```

Avoid comments like `i++  // increment i` or `if err != nil {  // check for errors` — the code speaks for itself.

#### Developer Annotations

Use markers consistently to flag non-obvious behavior, improvements, and known issues:

- **NOTE:** Explains a constraint, assumption, or non-obvious behavior that may surprise future readers.
- **TODO:** Marks incomplete implementations, planned improvements, or deferred work.
- **FIX:** Indicates a known bug, workaround, or limitation that should be addressed.

Examples:

```go
// NOTE: Thinking models require special handling; they stream content in the Thinking
// field instead of Response. We coalesce both into the final response.

// TODO: Implement exponential backoff for reconnection attempts.
// Currently retries every 5 seconds; on sustained outages, this may trigger rate limits.

// FIX: When message exceeds 2000 chars, Discord truncates silently instead of returning error.
// We detect and split manually, but this is a workaround for upstream API behavior.
```

#### Types & Structs

**Exported types:** Always doc comment with purpose and typical usage context.

```go
// Provider is the interface all LLM backends must implement.
// Implementations handle model-specific request/response marshalling and API communication.
type Provider interface {
    Generate(ctx context.Context, req Request) (*Response, error)
}

// Request represents a single LLM request with messages, parameters, and optional tools.
type Request struct {
    Model   string         // Model identifier (provider-specific)
    Messages []ChatMessage // Conversation history
    Tools   []Tool        // Optional tools the model can invoke; nil if not supported
}
```

**Unexported types in types.go files:** Add brief comments explaining role, even if private. Rationale: `types.go` files are conceptual type dictionaries; clarity aids understanding internal request/response mapping.

```go
// chatMessage is Ollama's internal message format; differs from the generic llm.ChatMessage
// in role naming and content structure. mapToOllamaMessages handles the conversion.
type chatMessage struct {
    Role    string `json:"role"`
    Content string `json:"content"`
}
```

**Struct fields:** Comment non-obvious or context-dependent fields; skip obvious ones:

```go
type Request struct {
    ID string              // Unique request identifier
    Payload json.RawMessage // Deferred decoding; format determined by Type field
    RetryCount int         // Number of retries attempted (0 on first attempt)
    Ctx context.Context    // Request context; cancellation stops the operation
}

// Skip comments for obvious fields:
type Person struct {
    Name string
    Age int
}
```

#### Deprecations

Always include migration guidance and timeline:

```go
// Deprecated: Use Chat instead. ChatRequest will be removed in v2.0.
// Migration: Replace `req := &ChatRequest{Model: "m"}` with `req := &Chat{Model: "m"}`.
type ChatRequest struct { ... }

// Deprecated: Use NewClient instead. CreateClient will be removed by end of 2026.
func CreateClient(url string) *Client { ... }
```

#### Common Patterns in Oswald AI

**Thinking Models:** Always document the special handling:

```go
// NOTE: Thinking models (e.g., o1) emit streaming content in Thinking, not Response.
// We aggregate Thinking chunks into the final Response for caller transparency.
```

**Callback Adaptation:** When bridging internal and external contracts:

```go
// Adapt internal callback ([]byte) to external (string) to avoid caller-side conversion.
streamCallback := func(content string) {
    internalCallback([]byte(content))
}
```

**Goroutine Launches:** Always document lifecycle and cancellation:

```go
// Launch heartbeat loop; runs until hb.Done closes or heartbeat interval expires.
go dg.heartbeatLoop(conn, hb)
```

---

## Project-Specific Patterns

### Adding a New LLM Provider

1. Create a new subdirectory under `internal/llm/` (e.g., `internal/llm/openai/`).
2. Implement the `llm.Provider` interface defined in `internal/llm/provider.go`.
3. Define provider-specific JSON structs in a `types.go` file in that package.
4. Wire the new provider into `cmd/agent/main.go`.

### Adding a New Gateway

1. Create a new file under `internal/gateway/` (e.g., `slack.go`).
2. Implement the `gateway.Service` interface defined in `internal/gateway/gateway.go`.
3. Register the gateway in `cmd/agent/main.go` alongside existing gateways.

### Routing / Triage

The triage system in `internal/agent/triage.go` uses an LLM call to classify incoming messages. The base system prompt for Oswald is defined in `internal/agent/decision.go` as `OswaldBasePrompt`. When modifying routing logic, update both the triage classification logic and the decision/routing table.

### Environment Configuration

All configuration is loaded from environment variables (with `.env` support via `godotenv`) in `internal/config/config.go`. Add new config fields there and use the `getEnv(key, default)` helper pattern already established in that file.

---

## Dependencies

| Package | Version | Purpose |
|---|---|---|
| `github.com/gorilla/websocket` | v1.5.3 | WebSocket client (Discord) and server (local WS) |
| `github.com/joho/godotenv` | v1.5.1 | `.env` file loading |
| Go stdlib | go 1.25+ | `net/http`, `encoding/json`, `context`, `log`, etc. |

No web framework is used — the HTTP server is raw `net/http`.

After adding or removing dependencies, always run `go mod tidy`.
