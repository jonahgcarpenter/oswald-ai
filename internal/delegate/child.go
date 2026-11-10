package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// childSpec carries everything one child turn needs. The manager fills it
// from the dispatch envelope; nested inline runs fill it from the call.
type childSpec struct {
	goal         string
	context      string
	outputSchema map[string]interface{}
	imageHints   []string
	model        string
	provider     string
	billingURL   string
	client       llm.Chatter
	role         string
	depth        int
	maxIter      int
	principal    identity.Principal
	sessionKey   string
	displayName  string
	enabledTools []string
	mcpServers   []string
	steer        *agent.SteerInbox
	requestID    string
	transcript   *transcript
}

// childResult is the terminal outcome of one child turn, serialized into
// result_json and rendered into the delivery message.
type childResult struct {
	Status        string       `json:"status"`
	Summary       string       `json:"summary"`
	Truncated     bool         `json:"truncated,omitempty"`
	SchemaValid   *bool        `json:"schema_valid,omitempty"`
	SchemaErrors  []string     `json:"schema_errors,omitempty"`
	Timeout       *timeoutInfo `json:"timeout,omitempty"`
	Orphans       []string     `json:"orphaned_processes,omitempty"`
	Unread        []string     `json:"unread_child_output,omitempty"`
	MissedSteer   []string     `json:"missed_steer,omitempty"`
	RouteModel    string       `json:"route_model,omitempty"`
	RouteProvider string       `json:"route_provider,omitempty"`
}

type timeoutInfo struct {
	Seconds      int64  `json:"timeout_seconds"`
	Elapsed      int64  `json:"timed_out_after_seconds"`
	LastEventAge int64  `json:"last_event_age_seconds"`
	Phase        string `json:"timeout_phase"`
}

// buildBrief assembles the child's user message: goal, context, and the
// contract. The child knows nothing else about the parent conversation.
func buildBrief(goal, contextText string, images []string, schema map[string]interface{}, maxIter int) string {
	var sb strings.Builder
	sb.WriteString("You are a subagent working a delegated task. Complete it and report back concisely: what you did, files you modified, and any issues found. You have at most ")
	sb.WriteString(strconv.Itoa(maxIter))
	sb.WriteString(" model responses; prioritize completing the goal over exhaustiveness.\n\n")
	sb.WriteString("Goal: " + strings.TrimSpace(goal) + "\n")
	if strings.TrimSpace(contextText) != "" {
		sb.WriteString("Context:\n" + strings.TrimSpace(contextText) + "\n")
	}
	for _, hint := range images {
		sb.WriteString("[Image attached at: " + hint + " — inspect it with vision_analyze before concluding anything about its contents.]\n")
	}
	if len(schema) > 0 {
		if raw, err := json.Marshal(schema); err == nil {
			sb.WriteString("Output contract: return ONLY valid JSON matching this schema, with no surrounding prose or code fences: " + string(raw) + "\n")
		}
	}
	sb.WriteString("Background processes you start die when you finish unless handed off; use process_manage handoff (at most 3) for anything that must survive, or wait for or kill the rest yourself.\n")
	return sb.String()
}

// runChild executes one synchronous child turn and returns its terminal
// result. Cancellation aborts with an interrupted result; provider errors
// fail closed with a single-line error. The caller owns delivery.
func (m *Manager) runChild(ctx context.Context, spec childSpec, rec *liveChild) childResult {
	routeModel, routeProvider := spec.model, spec.provider
	summary, truncated, status := m.runOnce(ctx, spec)
	result := childResult{Status: status, Summary: strings.TrimSpace(summary), Truncated: truncated, RouteModel: routeModel, RouteProvider: routeProvider}
	if status == "completed" && len(spec.outputSchema) > 0 {
		valid, schemaErrs := validateOutput(result.Summary, spec.outputSchema)
		if !valid {
			if corrected, ok := m.runCorrection(ctx, spec, schemaErrs); ok {
				result.Summary = strings.TrimSpace(corrected)
				valid, schemaErrs = validateOutput(corrected, spec.outputSchema)
			}
		}
		if valid {
			result.SchemaValid = boolPtr(true)
		} else {
			result.SchemaValid = boolPtr(false)
			result.SchemaErrors = schemaErrs
		}
	}
	result.Orphans, result.Unread = m.collectOrphans(spec.requestID)
	result.MissedSteer = missedSteers(rec)
	return result
}

// runOnce builds the child agent and runs a single turn to completion.
func (m *Manager) runOnce(ctx context.Context, spec childSpec) (summary string, truncated bool, status string) {
	child := agent.NewAgent(spec.client, m.registry, spec.model, spec.provider, m.soul, nil, budget.NewContextBudget(m.window), governance.DefaultGlobalPolicy(), m.log, m.mcp)
	child.SetWorkspace(m.profileRoot)
	child.SetImageCache(m.images)
	child.SetBillingBaseURL(spec.billingURL)
	req := agent.Request{
		RequestID:         spec.requestID,
		Principal:         spec.principal,
		DisplayName:       spec.displayName,
		SessionKey:        spec.sessionKey,
		Prompt:            buildBrief(spec.goal, spec.context, spec.imageHints, spec.outputSchema, spec.maxIter),
		SkipMemory:        true,
		EnabledToolsets:   spec.enabledTools,
		MCPServers:        spec.mcpServers,
		HideTools:         childBlockedTools(spec.role == "orchestrator" && m.orchestratorOn() && spec.depth+1 <= m.maxDepth()),
		MaxModelResponses: spec.maxIter,
		StreamFunc:        spec.transcript.stream,
		Steer:             spec.steer,
	}
	resp, err := child.Process(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, "interrupted"
		}
		return "Subagent failed: " + singleLine(err.Error()), false, "failed"
	}
	return strings.TrimSpace(resp.Response), resp.Truncated, "completed"
}

// correctionMaxIterations bounds the single correction turn after a schema
// miss. The main budget still applies underneath; this caps the retry
// itself so one bad contract cannot double the child's cost.
const correctionMaxIterations = 5

// runCorrection runs the single bounded correction turn after a schema miss,
// carrying only the validation errors (the schema is not re-pasted).
func (m *Manager) runCorrection(ctx context.Context, spec childSpec, schemaErrs []string) (string, bool) {
	var sb strings.Builder
	sb.WriteString("Your previous answer failed validation and will be discarded. Errors:\n")
	for _, e := range schemaErrs {
		sb.WriteString("- " + e + "\n")
	}
	sb.WriteString("Return ONLY the corrected JSON value matching the original contract, with no surrounding prose or code fences.")
	corrected := spec
	corrected.context = sb.String()
	corrected.outputSchema = nil
	corrected.maxIter = correctionMaxIterations
	if corrected.maxIter > spec.maxIter {
		corrected.maxIter = spec.maxIter
	}
	res := m.runChild(ctx, corrected, nil)
	if res.Status != "completed" || res.Summary == "" {
		return "", false
	}
	return res.Summary, true
}

// validateOutput checks a final answer against the declared JSON contract.
// Prose or a code fence around otherwise-valid JSON is tolerated.
func validateOutput(text string, schema map[string]interface{}) (bool, []string) {
	trimmed := strings.TrimSpace(text)
	if decoded, ok := tryParseJSON(trimmed); ok {
		return checkSchemaShape(decoded, schema)
	}
	start := strings.IndexAny(trimmed, "{[")
	end := strings.LastIndexAny(trimmed, "}]")
	if start >= 0 && end > start {
		if decoded, ok := tryParseJSON(trimmed[start : end+1]); ok {
			return checkSchemaShape(decoded, schema)
		}
	}
	return false, []string{"answer is not valid JSON"}
}

func tryParseJSON(text string) (interface{}, bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 3 {
			trimmed = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}
	var decoded interface{}
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

// checkSchemaShape verifies the decoded value is a JSON object. Structural
// validation stays shallow by design: the contract is a shape gate, and the
// parent extracts what it needs from the raw text on mismatch.
func checkSchemaShape(decoded interface{}, _ map[string]interface{}) (bool, []string) {
	if _, ok := decoded.(map[string]interface{}); !ok {
		return false, []string{"answer must be a JSON object"}
	}
	return true, nil
}

// missedSteers reports accepted steers that arrived after the child's last
// recorded tool result — the child finished before they could land.
func missedSteers(rec *liveChild) []string {
	if rec == nil || rec.activity == nil || len(rec.steers) == 0 {
		return nil
	}
	final := atomic.LoadInt64(&rec.activity.toolResults)
	var out []string
	for _, s := range rec.steers {
		if s.toolCalls >= final {
			out = append(out, s.text)
		}
	}
	return out
}

func boolPtr(v bool) *bool { return &v }

func singleLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 500 {
		return string(r[:500]) + "…"
	}
	return s
}

// transcript streams one child turn into a human-readable log for live
// inspection and stall detection. Tool names and outcomes are recorded;
// arguments and result bodies are not (never log payloads).
type transcript struct {
	mu      sync.Mutex
	file    *os.File
	path    string
	pending string
	act     *unitActivity
	written int64
	closed  bool
}

const maxTranscriptBytes = 256 << 10

// newTranscript creates the task log file, creating parent directories.
func newTranscript(path string) (*transcript, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	act := &unitActivity{lastEvent: time.Now().UnixNano(), startedAt: time.Now().UnixNano()}
	return &transcript{file: f, path: path, act: act}, nil
}

func (t *transcript) stamp() string {
	return time.Now().UTC().Format("15:04:05")
}

// stream is installed as the child turn's StreamFunc: every chunk updates
// activity tracking and appends one bounded line.
func (t *transcript) stream(chunk agent.StreamChunk) {
	if t == nil {
		return
	}
	now := time.Now().UnixNano()
	atomic.StoreInt64(&t.act.lastEvent, now)
	atomic.CompareAndSwapInt64(&t.act.firstEventAt, 0, now)
	var line string
	switch chunk.Type {
	case agent.ChunkThinking, agent.ChunkContent:
		t.mu.Lock()
		t.pending += chunk.Text
		t.mu.Unlock()
		return
	case agent.ChunkToolCall:
		name := ""
		if chunk.Tool != nil {
			name = chunk.Tool.Name
		}
		atomic.AddInt64(&t.act.toolCalls, 1)
		atomic.StoreInt64(&t.act.inTool, now)
		t.flushPending()
		line = "tool_call " + name
	case agent.ChunkToolResult:
		name, outcome := "", "ok"
		if chunk.Tool != nil {
			name = chunk.Tool.Name
			if chunk.Tool.IsError {
				outcome = "error"
			}
		}
		atomic.AddInt64(&t.act.toolResults, 1)
		atomic.StoreInt64(&t.act.inTool, 0)
		t.flushPending()
		line = "tool_result " + name + " " + outcome
	default:
		t.flushPending()
		line = "status " + chunk.Text
	}
	t.write("[" + t.stamp() + "] " + line)
}

// flushPending writes buffered model tokens as one bounded line.
func (t *transcript) flushPending() {
	t.mu.Lock()
	text := strings.TrimSpace(t.pending)
	t.pending = ""
	t.mu.Unlock()
	if text == "" {
		return
	}
	runes := []rune(text)
	if len(runes) > 4000 {
		text = string(runes[:4000]) + " […truncated]"
	}
	t.write("[" + t.stamp() + "] model: " + singleLine(text))
}

func (t *transcript) write(line string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.file == nil {
		return
	}
	if t.written >= maxTranscriptBytes {
		return
	}
	if t.written+int64(len(line)) > maxTranscriptBytes {
		line = "[transcript truncated: size bound reached]"
	}
	n, _ := t.file.WriteString(line + "\n")
	t.written += int64(n)
}

// close flushes pending tokens, writes the terminal marker, and closes.
func (t *transcript) close(status string) {
	if t == nil {
		return
	}
	t.flushPending()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	if t.file != nil {
		_, _ = t.file.WriteString("[" + t.stamp() + "] " + status + "\n")
		_ = t.file.Close()
		t.file = nil
	}
}
