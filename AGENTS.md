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

- Exported functions and types must have a doc comment: `// NewAgent initializes...`
- Use `// NOTE:`, `// TODO:`, `// FIX:` markers for developer annotations.
- Use section comments for logical blocks within long functions: `// Start Heartbeat`, `// Identify`.

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
