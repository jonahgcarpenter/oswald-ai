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
4. **Three-tier Memory Model** — soul identity, per-user persistent facts, and in-session conversation history (see below)
5. **Tools** — generic registry/bootstrap in `internal/tools/`, tool-specific logic in subpackages; context key helpers in `internal/tools/toolctx/`
6. **LLM Client** — Ollama-only client and schema in `internal/ollama/`

---

## Three-Tier Memory Model

Oswald maintains three distinct layers of memory, each serving a different question:

| Layer | Storage | Answers | Mutable by agent |
| ----- | ------- | ------- | ---------------- |
| **Soul** | `config/soul.md` (disk) | Who is Oswald? (identity, origin, personality, directives) | Yes — via `soul_memory` tool |
| **User memory** | `data/memory/users/<id>.md` (disk) | Who is this user? (name, preferences, facts they've stated) | Yes — via `persistent_memory` tool |
| **Chat history** | In-process memory store | What is this conversation about? | Implicitly (turn pairs appended each request) |

The soul file is read fresh on every request, so edits via the `soul_memory` tool take effect on the next message without a restart.

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

# Separate terminal
go run ./test/memory.go
```

These integration programs connect to `ws://localhost:8080/ws` and require Ollama to be reachable.

| File                  | Purpose                                               |
| --------------------- | ----------------------------------------------------- |
| `test/ttft.go`        | Benchmarks time to first token for streamed responses |
| `test/interactive.go` | Interactive prompting environment for testing         |
| `test/memory.go`      | Exercises TTL, max-turn retention, and context pruning via debug dumps |

When adding new integration checks, keep using the same standalone `package main` pattern inside `test/`.

---

## Code Architecture

### Single Agentic Loop

The core runtime path is `(*Agent).Process()` in `internal/agent/agent.go`.

Flow:

1. Read `config/soul.md` fresh via the soul store — this becomes the system prompt
2. Retrieve conversation history for the session from `internal/memory`
3. Build the initial chat request with:
   - the current soul content (plus injected date/time)
   - the retrieved conversation history (user/assistant turn pairs), first pruned by memory retention policy and then compacted in-memory if needed to fit the active model's context budget
   - the current user prompt
   - all registered tools from the tool registry
4. Send the request to Ollama via `internal/ollama`
5. If the model emits tool calls:
   - execute each registered tool handler
   - append tool results as `tool` messages
   - repeat until the model stops calling tools, the request times out, or consecutive tool failures exhaust `MAX_TOOL_FAILURE_RETRIES`
6. Persist the user prompt and final assistant response to memory (tool intermediaries excluded)
7. Return the final `AgentResponse`, including optional streamed thinking/content chunks and model metrics

### Conversation Memory

Session state is managed by `internal/memory.Store`.

- Each session is identified by a `SessionKey` string set at the gateway level
- Only user and assistant final messages are stored — tool call intermediaries are excluded to keep history lean
- Session history is retained in memory until process exit, subject to optional TTL and max-turn retention limits
- `MEMORY_MAX_TURNS` retains only the newest N stored turn pairs per session when set above `0`
- `MEMORY_MAX_AGE` expires stored turn pairs older than the configured Go duration when set above `0`
- History is pruned for retention in `internal/memory`; if the retained history still exceeds the active prompt budget, the oldest retained prefix is compacted into a normal replacement turn pair and written back into memory with a fresh timestamp
- An empty `SessionKey` disables memory for a request (stateless one-shot behaviour)

### Context Budgeting

Before each `/api/chat` call, the agent estimates the prompt size after TTL/max-turn pruning. If the prompt would exceed budget, it compacts the oldest retained turn pairs into a single replacement turn pair that remains in ordinary chat history.

- Model metadata is discovered from Ollama's `/api/show` endpoint at startup
- `num_ctx` from the model parameters is preferred when available
- Model metadata `*.context_length` is used as a fallback
- Reserve values (`response_reserve`, `tool_reserve`, `safety_margin`) fall back to hardcoded package defaults when Ollama metadata provides no override
- Prompt compaction is destructive: the compacted replacement turn pair is written back into `internal/memory`, gets a fresh TTL timestamp, and still counts toward `MEMORY_MAX_TURNS`

**Hybrid Session Key Strategy (Discord gateway):**

| Context | Session Key | Behaviour |
| ------- | ----------- | --------- |
| DM | `SenderID` | Continuous per-user memory across all DMs |
| Guild channel / thread | `ChannelID:SenderID` | Per-user isolation; prevents cross-talk between users in the same channel |

**WebSocket gateway:** uses the connection's remote address as the session key, giving each connected client its own conversation memory for the connection lifetime.

Important runtime limits:

- `MAX_TOOL_FAILURE_RETRIES` defaults to `3`
- successful tool calls are not capped per request; only consecutive tool execution failures are limited
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

Current builtin tools:

- `web_search` in `internal/tools/websearch/`
- `persistent_memory` in `internal/tools/usermemory/`
- `soul_memory` in `internal/tools/soulmemory/`

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

| File                                        | Purpose                                                            |
| ------------------------------------------- | ------------------------------------------------------------------ |
| `cmd/agent/main.go`                         | Entrypoint: load config, bootstrap tools and gateways, start agent |
| `internal/agent/agent.go`                   | Core agentic tool-calling loop and response assembly               |
| `internal/config/config.go`                 | Environment config loading                                         |
| `internal/memory/store.go`                  | In-memory conversation store with TTL/max-turn retention, destructive compaction, and history flattening |
| `internal/debug/prompt.go`                  | Markdown prompt debug dump writer for request inspection |
| `internal/ollama/client.go`                 | Ollama HTTP client with chat streaming and tool support            |
| `internal/tools/registry.go`                | Generic tool registry and markdown parsing                         |
| `internal/tools/bootstrap.go`               | Builtin tool bootstrap                                             |
| `internal/tools/websearch/client.go`        | SearXNG-backed web search client                                   |
| `internal/tools/websearch/websearch.go`     | Web search handler + shared search types                           |
| `internal/tools/usermemory/store.go`        | Persistent per-user Markdown file store with per-user locking      |
| `internal/tools/usermemory/usermemory.go`   | `persistent_memory` tool handler (remember / recall / forget)      |
| `internal/tools/soulmemory/store.go`        | Single-file soul store with RWMutex for safe concurrent access     |
| `internal/tools/soulmemory/soulmemory.go`   | `soul_memory` tool handler (read / write / append)                 |
| `internal/tools/toolctx/toolctx.go`         | Context key helpers for sender ID injection into tool handlers     |
| `internal/gateway/bootstrap.go`             | Enabled gateway assembly from config                               |
| `internal/gateway/discord/gateway.go`       | Discord runtime behavior                                           |
| `internal/gateway/discord/types.go`         | Discord constants and payload structs                              |
| `internal/gateway/websocket/gateway.go`     | Local WebSocket runtime behavior                                   |
| `internal/gateway/websocket/types.go`       | WebSocket gateway shared types including `IncomingMessage`         |
| `config/soul.md`                            | Agent identity and personality (system prompt); editable at runtime |
| `config/tools/*.md`                         | Tool schemas exposed to the model                                  |

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
- `Info` — production-facing operational milestones; startup, enabled tools, enabled gateways, selected model
- `Warn` — degraded but recoverable behavior; skipped work, retries, parse failures, tool failures, soul file modifications
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

### Changing the Model

Set the `OLLAMA_MODEL` environment variable. The model name is passed directly to Ollama — any model available in your local Ollama instance can be used.

### Changing Oswald's Personality

Edit `config/soul.md` directly, or let the agent do it via the `soul_memory` tool. The file is read fresh on every request, so changes take effect immediately without a restart.

### Web Search Configuration

- SearXNG base URL comes from `SEARXNG_URL`
- consecutive tool failure retry limit comes from `MAX_TOOL_FAILURE_RETRIES`

---

## Debugging Tips

### Enable Debug Logging

```bash
LOG_LEVEL=debug go run ./cmd/agent/main.go
```

Useful debug output includes:

- request processing
- memory retention pruning and context-budget pruning
- tool execution
- agentic loop iteration counts
- websocket and Discord request handling
- web search query execution
- soul file read/write operations

### Prompt Debug Dumps

Set `PROMPT_DEBUG_PATH` to write a timestamped Markdown file for every request.

- metadata: model, session, token budget, and compaction estimates
- session history: retained conversation history after any in-memory compaction
- final message array: the exact messages sent to Ollama
- tool schemas: every tool definition included in the request

### Common Verification Commands

```bash
gofmt -w .
go list ./... | grep -v '/test$' | xargs go vet
go build -o ./tmp/main ./cmd/agent/main.go
```

---

## Environment Configuration

| Variable                 | Default                  | Purpose                              |
| ------------------------ | ------------------------ | ------------------------------------ |
| `PORT`                   | `8080`                   | WebSocket gateway port               |
| `OLLAMA_URL`             | `http://localhost:11434` | Ollama API                           |
| `OLLAMA_MODEL`           | *(required)*             | Ollama model name; startup fails if empty |
| `SEARXNG_URL`            | `http://localhost:8888`  | SearXNG API                          |
| `DISCORD_TOKEN`          | empty                    | Enables Discord gateway              |
| `MAX_TOOL_FAILURE_RETRIES` | `3`                    | Max consecutive tool execution failures before tools are disabled for the request |
| `LOG_LEVEL`              | `info`                   | Logging verbosity                    |
| `MEMORY_MAX_TURNS`       | `10`                     | Max retained memory turn pairs per session; `0` disables the cap |
| `MEMORY_MAX_AGE`         | `0`                      | Max retained memory age as Go duration (for example `24h`); `0` disables expiry |
| `PROMPT_DEBUG_PATH`      | empty                    | Directory for per-request Markdown prompt dumps; each request writes a timestamped `.md` file showing retained session history, the final message array, tool schemas, and context budget |

Paths for the soul file, tool definitions, and persistent user memory are hardcoded in `internal/config/config.go`.

---

## Dependencies

| Package                        | Purpose                                             |
| ------------------------------ | --------------------------------------------------- |
| `github.com/gorilla/websocket` | Discord WebSocket client and local WebSocket server |
| `github.com/joho/godotenv`     | `.env` loading                                      |

After dependency changes, run `go mod tidy`.

---

## Known Limitations

- No persistent conversation history (conversation memory is in-process only; restarts clear all sessions — per-user fact memory via `persistent_memory` tool and soul identity via `soul_memory` tool both survive restarts)
- No session resumption for Discord reconnects
- WebSocket API has no authentication layer
- Three builtin tools: `web_search`, `persistent_memory`, and `soul_memory`
- Ollama is the only model backend

---

**Oswald AI** — a local Go agent with tools, gateways, and no patience.
