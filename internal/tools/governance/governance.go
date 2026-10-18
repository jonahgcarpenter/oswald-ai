// Package governance provides request-local limits and outcome tracking for
// model-visible tool execution.
package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

// Outcome classifies a successful tool execution by whether it produced useful
// information for the active request.
type Outcome string

const (
	OutcomeProductive   Outcome = "productive"
	OutcomeUnproductive Outcome = "unproductive"
)

// Result is the shared result returned by builtin and MCP tools.
type Result struct {
	// ResultName is an optional display/history name distinct from the invocation.
	ResultName  string
	Content     string
	Outcome     Outcome
	ReasonCode  string
	IsDegraded  bool
	Attachments []media.OutputAttachment
	// Images are request-local model inputs, never outbound attachments or history.
	Images []llm.InputImage `json:"-"`
}

// HistoryMode controls how much of a completed tool call may be retained in
// durable session history.
type HistoryMode string

const (
	HistoryNone     HistoryMode = "none"
	HistoryMetadata HistoryMode = "metadata"
	HistoryFull     HistoryMode = "full"
)

const (
	defaultHistoryArgumentBytes = 16 * 1024
	defaultHistoryResultRunes   = 16_000
)

// HistoryPolicy controls durable and searchable tool-call history. An empty
// mode defaults to full history so existing registered tools retain continuity.
type HistoryPolicy struct {
	Mode             HistoryMode
	SearchResult     bool
	MaxArgumentBytes int
	MaxResultRunes   int
}

// Effective returns the policy with compatibility defaults applied.
func (p HistoryPolicy) Effective() HistoryPolicy {
	if p.Mode == "" {
		p.Mode = HistoryFull
		p.SearchResult = true
	}
	if p.MaxArgumentBytes == 0 {
		p.MaxArgumentBytes = defaultHistoryArgumentBytes
	}
	if p.MaxResultRunes == 0 {
		p.MaxResultRunes = defaultHistoryResultRunes
	}
	return p
}

// ArgumentNormalizer returns the semantic argument value used for duplicate
// detection. It must not retain or mutate the supplied map.
type ArgumentNormalizer func(map[string]interface{}) interface{}

// ToolPolicy controls request-local history retention for one model-visible
// tool. Execution itself is never blocked per tool: failures and empty
// results return to the model with loop-guardrail advice, and only the
// request-wide global budget can stop the loop.
type ToolPolicy struct {
	NormalizeArgs ArgumentNormalizer
	History       HistoryPolicy
}

// Validate rejects invalid history modes and limits.
func (p ToolPolicy) Validate() error {
	history := p.History.Effective()
	if history.Mode != HistoryNone && history.Mode != HistoryMetadata && history.Mode != HistoryFull {
		return fmt.Errorf("invalid history mode %q", history.Mode)
	}
	if history.MaxArgumentBytes < 0 || history.MaxResultRunes < 0 {
		return fmt.Errorf("history limits must not be negative")
	}
	return nil
}

// GlobalPolicy controls request-wide tool-loop limits.
type GlobalPolicy struct {
	MaxExecutions     int
	MaxToolIterations int
}

// DefaultGlobalPolicy returns the fixed request-wide safety ceilings.
func DefaultGlobalPolicy() GlobalPolicy {
	return GlobalPolicy{MaxExecutions: 50, MaxToolIterations: 30}
}

// Validate checks the request-wide limits.
func (p GlobalPolicy) Validate() error {
	if p.MaxExecutions <= 0 {
		return fmt.Errorf("max executions must be positive")
	}
	if p.MaxToolIterations <= 0 {
		return fmt.Errorf("max tool iterations must be positive")
	}
	return nil
}

// ToolStats contains request-local counters for one tool.
type ToolStats struct {
	Attempts     int
	Executions   int
	Productive   int
	Unproductive int
	Failures     int
	Duplicates   int
	Blocked      int
}

// Decision is the governor's pre-execution decision for one model-emitted call.
type Decision struct {
	Allowed     bool
	ReasonCode  string
	fingerprint string
}

// Fingerprint returns the request-local duplicate key recorded for an allowed
// decision, or "" when the decision carries no fingerprint.
func (d Decision) Fingerprint() string { return d.fingerprint }

const (
	ReasonGlobalLimit    = "global_execution_limit"
	ReasonIterationLimit = "global_iteration_limit"
	ReasonUnadvertised   = "tool_not_advertised"
)

// Governor tracks one request's tool usage and request-wide budget. It is
// intentionally not safe for concurrent use because Agent.Process executes
// one tool batch serially. Per-tool blocking is gone: every advertised call
// executes, and loop-guardrail ladders advise instead of refusing.
type Governor struct {
	global          GlobalPolicy
	stats           map[string]*ToolStats
	toolIterations  int
	totalExecutions int
	globalReason    string

	// Loop-guardrail state. Failure/no-progress ladders track repeated
	// signatures so exact-error replays warn instead of retrying blindly.
	exactFailures    map[string]int
	sameToolFailures map[string]int
	noProgress       map[string]noProgressEntry
	identicalSig     string
	identicalHash    string
	identicalCount   int
	identicalFirstID string
	history          []historyEntry
	haltDecision     *Decision
	hardStop         bool
}

// New creates request-local governance state.
func New(global GlobalPolicy) *Governor {
	return &Governor{
		global:           global,
		stats:            make(map[string]*ToolStats),
		exactFailures:    make(map[string]int),
		sameToolFailures: make(map[string]int),
		noProgress:       make(map[string]noProgressEntry),
	}
}

// SetHardStopEnabled toggles blocking/halt behavior. It stays false in v1
// (warn-only); phase 3 flips it on unattended paths after log observation.
func (g *Governor) SetHardStopEnabled(enabled bool) { g.hardStop = enabled }

// HaltDecision returns the first block/halt decision, if any.
func (g *Governor) HaltDecision() *Decision { return g.haltDecision }

// BeginToolIteration records a model response containing at least one tool call.
func (g *Governor) BeginToolIteration() Decision {
	g.toolIterations++
	if g.toolIterations > g.global.MaxToolIterations {
		g.setGlobalReason(ReasonIterationLimit)
		return Decision{ReasonCode: ReasonIterationLimit}
	}
	return Decision{Allowed: true}
}

// BeforeExecution determines whether one advertised model call may execute.
// Only the request-wide budget and the advertised catalog gate execution:
// per-tool failures, empty results, and repeats always execute.
func (g *Governor) BeforeExecution(name string, args map[string]interface{}, policy ToolPolicy, advertised bool) Decision {
	stats := g.toolStats(name)
	stats.Attempts++
	if !advertised {
		stats.Blocked++
		return Decision{ReasonCode: ReasonUnadvertised}
	}
	if g.globalReason != "" {
		stats.Blocked++
		return Decision{ReasonCode: g.globalReason}
	}
	if g.totalExecutions >= g.global.MaxExecutions {
		g.setGlobalReason(ReasonGlobalLimit)
		stats.Blocked++
		return Decision{ReasonCode: ReasonGlobalLimit}
	}
	decision := Decision{Allowed: true}
	// Fingerprints still feed the loop-guardrail ladders; repeats execute.
	// The hard-stop ladder blocks below stay dormant until hard-stop is
	// enabled.
	fingerprint, ferr := Fingerprint(name, args, policy.NormalizeArgs)
	if ferr != nil {
		fingerprint = ""
	}
	if g.hardStop && fingerprint != "" {
		if count := g.exactFailures[fingerprint]; count >= ExactBlockAfter {
			stats.Blocked++
			blocked := Decision{ReasonCode: ReasonRepeatedExactBlock}
			g.setHaltDecision(&blocked)
			return blocked
		}
		if isIdempotent(name) {
			if entry, ok := g.noProgress[fingerprint]; ok && entry.count >= NoProgressBlockAfter {
				stats.Blocked++
				blocked := Decision{ReasonCode: ReasonIdempotentNoProgressBlock}
				g.setHaltDecision(&blocked)
				return blocked
			}
		}
	}
	decision.fingerprint = fingerprint
	stats.Executions++
	g.totalExecutions++
	return decision
}

// RecordResult accounts for a completed permitted execution. It preserves the
// legacy statistics contract and updates loop-guardrail ladders; advice is
// discarded. Prefer AfterCall when model-visible warn text is needed.
func (g *Governor) RecordResult(name string, decision Decision, result Result, execErr error) {
	g.AfterCall(name, nil, decision.fingerprint, result, execErr)
}

// GlobalStopReason returns the reason all tools must be removed, if any.
func (g *Governor) GlobalStopReason() string {
	if g.globalReason == "" {
		switch {
		case g.totalExecutions >= g.global.MaxExecutions:
			g.setGlobalReason(ReasonGlobalLimit)
		case g.toolIterations >= g.global.MaxToolIterations:
			g.setGlobalReason(ReasonIterationLimit)
		}
	}
	if g.globalReason == "" && g.haltDecision != nil {
		return g.haltDecision.ReasonCode
	}
	return g.globalReason
}

// Stats returns a copy of one tool's counters.
func (g *Governor) Stats(name string) ToolStats { return *g.toolStats(name) }

// TotalExecutions returns the number of handlers invoked in this request.
func (g *Governor) TotalExecutions() int { return g.totalExecutions }

// ToolIterations returns the number of model responses containing tool calls.
func (g *Governor) ToolIterations() int { return g.toolIterations }

// ExactFailures returns the exact-signature failure count for a fingerprint.
func (g *Governor) ExactFailures(fingerprint string) int { return g.exactFailures[fingerprint] }

// SameToolFailures returns the same-tool failure count for a tool name.
func (g *Governor) SameToolFailures(name string) int { return g.sameToolFailures[name] }

func (g *Governor) toolStats(name string) *ToolStats {
	stats := g.stats[name]
	if stats == nil {
		stats = &ToolStats{}
		g.stats[name] = stats
	}
	return stats
}

func (g *Governor) setGlobalReason(reason string) {
	if g.globalReason == "" {
		g.globalReason = reason
	}
}

func (g *Governor) setHaltDecision(decision *Decision) {
	if g.haltDecision == nil && decision != nil {
		cp := *decision
		g.haltDecision = &cp
	}
}

// Fingerprint returns a request-local-safe digest for a tool name and canonical
// argument value. encoding/json deterministically sorts string map keys.
func Fingerprint(name string, args map[string]interface{}, normalize ArgumentNormalizer) (string, error) {
	var value interface{} = args
	if normalize != nil {
		value = normalize(args)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode normalized tool arguments: %w", err)
	}
	digest := sha256.Sum256(append(append([]byte(name), 0), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

// classifyCall separates execution failures from unproductive successes.
// Degraded successes (e.g. web_search fallback) are neither.
func classifyCall(execErr error, result Result) (failed, unproductive bool) {
	if execErr != nil {
		return true, false
	}
	if result.Outcome != OutcomeProductive {
		return false, true
	}
	return false, false
}

func isRepeatablePoller(name string) bool {
	if name == "process_manage" {
		return true
	}
	for _, suffix := range []string{"_poll", "_get_result", "_status", "_progress"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func isIdempotent(name string) bool {
	switch name {
	case "session_search", "web_search", "vision_analyze":
		return true
	}
	return false
}

func isFailureTolerant(name string) bool {
	if name == "web_search" || name == "tool_call" || name == "tool_search" || name == "tool_describe" {
		return true
	}
	return strings.HasPrefix(name, "mcp__")
}

func isProgressReset(name string) bool {
	return name == "memory" || name == "image_generate"
}
