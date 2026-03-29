# AGENTS.md — Oswald AI Developer Reference

This document captures the current structure and conventions for working on Oswald AI.

---

## Project Overview

Oswald AI is a pure Go application.
It runs a single agentic chat loop on top of Ollama, exposes that loop through Discord and a local WebSocket gateway, and supports builtin tools such as web search.

There is no JavaScript, TypeScript, or frontend code in this repository.

Current architecture layers:

1. **Gateway bootstrap** — shared gateway interface + startup wiring in `internal/gateway/`
2. **Gateway implementations** — Discord and local WebSocket in `internal/gateway/discord/` and `internal/gateway/websocket/`
3. **Agent/Orchestration** — single tool-calling loop in `internal/agent/`
4. **Tools** — generic registry/bootstrap in `internal/tools/`, tool-specific logic in subpackages such as `internal/tools/websearch/`
5. **LLM Client** — Ollama-only client and schema in `internal/ollama/`

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

# Vet non-test packages
go list ./... | grep -v '/test$' | xargs go vet

# Tidy module dependencies
go mod tidy

# Build Docker image
docker build -t oswald-ai .
```

The repo contains standalone integration programs in `test/`, so `go vet ./...` and `go test ./...` are not the most useful verification commands for day-to-day work.

---

## Testing

There are no `*_test.go` files yet.
Integration tests are standalone `package main` programs in `test/`:

```bash
# Start server first
go run ./cmd/agent/main.go

# Separate terminal
go run ./test/ttft.go

# Separate terminal
go run ./test/interactive.go
```

Both tests connect to `ws://localhost:8080/ws` and require Ollama to be reachable.

| File                  | Purpose                                               |
| --------------------- | ----------------------------------------------------- |
| `test/ttft.go`        | Benchmarks time to first token for streamed responses |
| `test/interactive.go` | Interactive prompting environment for testing         |

When adding new integration checks, keep using the same standalone `package main` pattern inside `test/`.

---

## Code Architecture

### Single Agentic Loop

The core runtime path is `(*Agent).Process()` in `internal/agent/agent.go`.

Flow:

1. Build the initial chat request with:
   - the configured system prompt from `config/workers.yaml`
   - the user prompt
   - all registered tools from the tool registry
2. Send the request to Ollama via `internal/ollama`
3. If the model emits tool calls:
   - execute each registered tool handler
   - append tool results as `tool` messages
   - repeat until the model stops calling tools or `MAX_ITERATIONS` is reached
4. Return the final `AgentResponse`, including optional streamed thinking/content chunks and model metrics

Important runtime limits:

- `MAX_ITERATIONS` defaults to `5`
- total executed tool calls are capped by `maxIntelResults` in `internal/agent/agent.go`
- overall request timeout is `3*time.Minute` in `Process()`

### Tools

Tools are split into two layers:

- `internal/tools/`
  - generic registry
  - markdown parsing
  - bootstrap wiring
- `internal/tools/<toolname>/`
  - tool-specific logic
  - external API clients
  - handler creation

Current builtin tool:

- `web_search` in `internal/tools/websearch/`

Tool definitions are loaded from `config/tools/*.md`.

### Gateways

Gateway structure is now:

- `internal/gateway/gateway.go` — shared `Service` interface
- `internal/gateway/bootstrap.go` — config-based gateway assembly
- `internal/gateway/discord/` — Discord gateway implementation
- `internal/gateway/websocket/` — local WebSocket gateway implementation

`cmd/agent/main.go` should only depend on the root `internal/gateway` package, not concrete gateway subpackages.

### Ollama

Ollama is the only LLM integration in this project.

Relevant files:

- `internal/ollama/client.go`
- `internal/ollama/schema.go`
- `internal/ollama/types.go`

There is no longer a generic provider abstraction in the codebase.

---

## Key Files & Responsibilities

| File                                    | Purpose                                                            |
| --------------------------------------- | ------------------------------------------------------------------ |
| `cmd/agent/main.go`                     | Entrypoint: load config, bootstrap tools and gateways, start agent |
| `internal/agent/agent.go`               | Core agentic tool-calling loop and response assembly               |
| `internal/agent/workers.go`             | Load `config/workers.yaml` and resolve workers                     |
| `internal/config/config.go`             | Environment config loading                                         |
| `internal/ollama/client.go`             | Ollama HTTP client with chat streaming and tool support            |
| `internal/tools/registry.go`            | Generic tool registry and markdown parsing                         |
| `internal/tools/bootstrap.go`           | Builtin tool bootstrap                                             |
| `internal/tools/websearch/client.go`    | SearXNG-backed web search client                                   |
| `internal/tools/websearch/websearch.go` | Web search handler + shared search types                           |
| `internal/gateway/bootstrap.go`         | Enabled gateway assembly from config                               |
| `internal/gateway/discord/gateway.go`   | Discord runtime behavior                                           |
| `internal/gateway/discord/types.go`     | Discord constants and payload structs                              |
| `internal/gateway/websocket/gateway.go` | Local WebSocket runtime behavior                                   |
| `internal/gateway/websocket/types.go`   | WebSocket gateway shared types                                     |
| `config/workers.yaml`                   | Model/system prompt config                                         |
| `config/tools/*.md`                     | Tool schemas exposed to the model                                  |

---

## Code Style Guidelines

### Formatting

- Use `gofmt`
- Use tabs as enforced by `gofmt`
- Run scoped vet for normal verification: `go list ./... | grep -v '/test$' | xargs go vet`

### Import Organization

Use three import groups separated by blank lines:

1. stdlib
2. third-party
3. internal packages

### Naming Conventions

| Element                  | Convention                       | Example                                        |
| ------------------------ | -------------------------------- | ---------------------------------------------- |
| Packages                 | short, lowercase, no underscores | `agent`, `gateway`, `ollama`, `websearch`      |
| Exported types           | `PascalCase`                     | `AgentResponse`, `ModelMetrics`, `Registry`    |
| Unexported types         | `camelCase`                      | `workerFile`                                   |
| Interfaces               | semantic `PascalCase`            | `Service`, `Searcher`, `Chatter`               |
| Exported funcs/methods   | `PascalCase`                     | `NewAgent`, `Process`, `NewRegistryFromConfig` |
| Unexported funcs/methods | `camelCase`                      | `connectAndListen`, `registerBuiltins`         |

Constructors should use `NewX` and generally return pointers.

### Error Handling

Use standard Go error handling and wrap with `%w` when adding context:

```go
conn, _, err := websocket.DefaultDialer.Dial(gatewayURL, nil)
if err != nil {
    return fmt.Errorf("failed to dial Discord Gateway: %w", err)
}
```

- Startup failures: `log.Fatal` in `main.go`
- Recoverable runtime problems: logger `Warn` / `Error`
- Avoid `panic` for expected error paths

### Logging Conventions

Logging levels are intentionally opinionated:

- `Debug` — noisy by design; request details, tool execution, iteration state, connection churn
- `Info` — production-facing operational milestones; startup, enabled tools, enabled gateways, selected worker model
- `Warn` — degraded but recoverable behavior; skipped work, retries, parse failures, tool failures
- `Error` — request or runtime failures that prevented intended behavior

Do not put per-request or per-token chatter at `Info` level.

### Concurrency

- Use goroutines for long-lived gateway loops, Discord heartbeats, and typing indicators
- Pass loop variables into goroutine closures explicitly
- Use `chan struct{}` for lightweight lifecycle signaling
- Use `chan os.Signal` in `main.go` for shutdown handling

### Comments

- Exported types and funcs need doc comments
- Explain intent and constraints, not syntax
- Prefer comments that explain **why**

---

## Project-Specific Patterns

### Adding a New Tool

1. Add a markdown tool schema in `config/tools/<name>.md`
2. Create `internal/tools/<name>/` if the tool needs runtime logic
3. Add handler wiring in `internal/tools/bootstrap.go`
4. Keep generic registry logic in `internal/tools/`; keep tool-specific code in the tool subpackage

### Adding a New Gateway

1. Create a new implementation package under `internal/gateway/<name>/`
2. Implement the `gateway.Service` interface from `internal/gateway/gateway.go`
3. Wire it in `internal/gateway/bootstrap.go`
4. Do not wire concrete gateways directly in `cmd/agent/main.go`

### Changing Model Behavior

Edit `config/workers.yaml`.

At the moment, the only required worker is `GENERAL`, and it drives the entire runtime loop.

### Web Search Configuration

- SearXNG base URL comes from `SEARXNG_URL`
- Tool-call iteration limit comes from `MAX_ITERATIONS`
- tool-result cap is `maxIntelResults` in `internal/agent/agent.go`

---

## Debugging Tips

### Enable Debug Logging

```bash
LOG_LEVEL=debug go run ./cmd/agent/main.go
```

Useful debug output includes:

- request processing
- tool execution
- agentic loop iteration counts
- websocket and Discord request handling
- web search query execution

### Common Verification Commands

```bash
gofmt -w .
go list ./... | grep -v '/test$' | xargs go vet
go build -o ./tmp/main ./cmd/agent/main.go
```

---

## Environment Configuration

| Variable         | Default                  | Purpose                 |
| ---------------- | ------------------------ | ----------------------- |
| `PORT`           | `8080`                   | WebSocket gateway port  |
| `OLLAMA_URL`     | `http://localhost:11434` | Ollama API              |
| `SEARXNG_URL`    | `http://localhost:8888`  | SearXNG API             |
| `WORKERS_CONFIG` | `config/workers.yaml`    | Worker definitions      |
| `TOOLS_CONFIG`   | `config/tools`           | Tool definitions        |
| `DISCORD_TOKEN`  | empty                    | Enables Discord gateway |
| `MAX_ITERATIONS` | `5`                      | Agentic loop cap        |
| `LOG_LEVEL`      | `info`                   | Logging verbosity       |

Implementation lives in `internal/config/config.go` using `godotenv.Load()` plus `os.LookupEnv()`.

---

## Dependencies

| Package                        | Purpose                                             |
| ------------------------------ | --------------------------------------------------- |
| `github.com/gorilla/websocket` | Discord WebSocket client and local WebSocket server |
| `github.com/joho/godotenv`     | `.env` loading                                      |
| `gopkg.in/yaml.v3`             | Worker config parsing                               |

After dependency changes, run `go mod tidy`.

---

## Known Limitations

- No persistent conversation history
- No session resumption for Discord reconnects
- WebSocket API has no authentication layer
- Only one builtin tool is currently implemented: `web_search`
- Ollama is the only model backend

---

**Oswald AI** — a local Go agent with tools, gateways, and no patience.
