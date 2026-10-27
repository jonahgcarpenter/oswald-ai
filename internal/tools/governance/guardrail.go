package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Loop-guardrail thresholds. Hardcoded; no operator configuration surface.
const (
	ExactWarnAfter       = 2
	ExactBlockAfter      = 5
	SameWarnAfter        = 3
	SameHaltAfter        = 8
	NoProgressWarnAfter  = 2
	NoProgressBlockAfter = 5
	IdenticalNoticeAfter = 3
	// NoProgressWindow halts a request after this many consecutive completed
	// calls with no productive result across all tools. It catches varied
	// but useless work (different failing tools, different empty queries)
	// that no single-signature ladder sees. Any productive or
	// degraded-delivering result resets the streak; repeatable pollers never
	// increment it.
	NoProgressWindow     = 12
	CycleMinPeriod       = 2
	CycleMaxPeriod       = 4
	CycleHistory         = 64
	StubMinChars         = 512
	StubArgsPreviewRunes = 120
)

// Guardrail reason codes. Warn codes are model-visible advice; block/halt
// codes gate execution and stop the request loop.
const (
	ReasonRepeatedExactBlock        = "repeated_exact_failure_block"
	ReasonRepeatedExactWarning      = "repeated_exact_failure_warning"
	ReasonSameToolWarning           = "same_tool_failure_warning"
	ReasonSameToolHalt              = "same_tool_failure_halt"
	ReasonIdempotentNoProgressWarn  = "idempotent_no_progress_warning"
	ReasonIdempotentNoProgressBlock = "idempotent_no_progress_block"
	ReasonIdenticalStreakHalt       = "identical_call_streak_halt"
	ReasonIdenticalCycleHalt        = "identical_cycle_halt"
	ReasonNoProgressWindowHalt      = "no_progress_window_halt"
)

// GuardrailAdvice carries warn/block/halt guidance for one completed call.
// Warn appends model-visible advice; Block/Halt stop the request loop.
type GuardrailAdvice struct {
	Action  string // "", "warn", "block", "halt"
	Code    string
	Count   int
	Tool    string
	Message string
}

// Warn reports whether advice should be appended to the transient tool result.
func (a GuardrailAdvice) Warn() bool { return a.Action == "warn" && a.Code != "" }

// ObserveResult carries streak/cycle notices and large-duplicate stubs.
// All are transient model-visible only; durable history keeps full text.
type ObserveResult struct {
	Notice string
	Stub   string
	Kind   string
}

type noProgressEntry struct {
	hash  string
	count int
}

type historyEntry struct {
	sig        string
	hash       string
	repeatable bool
}

// AfterCall updates legacy statistics and loop-guardrail ladders for one
// completed permitted execution, then returns warn/block/halt advice.
// failed counts only execErr != nil; unproductive counts only successful
// non-productive outcomes; degraded-delivering success counts as productive
// progress and resets the no-progress window.
func (g *Governor) AfterCall(name string, args map[string]interface{}, fp string, result Result, execErr error) GuardrailAdvice {
	stats := g.toolStats(name)
	failed, unproductive := classifyCall(execErr, result)
	if failed {
		stats.Failures++
	} else if unproductive {
		stats.Unproductive++
	} else {
		stats.Productive++
	}

	// Sliding no-progress window: any productive delivery resets the streak;
	// failures and unproductive successes advance it (pollers excluded).
	// This is the catch-all for varied-but-useless work no single ladder sees.
	if !failed && !unproductive {
		g.nonProductiveStreak = 0
	} else if !isRepeatablePoller(name) {
		g.nonProductiveStreak++
	}

	if failed {
		if fp != "" {
			g.exactFailures[fp]++
		}
		g.sameToolFailures[name]++
		for _, inner := range innerRemoteNames(args) {
			if inner != "" && inner != name {
				g.sameToolFailures[inner]++
			}
		}
		if fp != "" {
			delete(g.noProgress, fp)
		}
		if !isFailureTolerant(name) {
			if g.sameToolFailures[name] >= SameHaltAfter {
				halt := Decision{ReasonCode: ReasonSameToolHalt}
				g.setHaltDecision(&halt)
				return GuardrailAdvice{Action: "halt", Code: ReasonSameToolHalt, Count: g.sameToolFailures[name], Tool: name, Message: haltMessage(name, g.sameToolFailures[name])}
			}
		}
		if fp != "" {
			if count := g.exactFailures[fp]; count >= ExactWarnAfter {
				if count >= ExactBlockAfter {
					block := Decision{ReasonCode: ReasonRepeatedExactBlock}
					g.setHaltDecision(&block)
					return GuardrailAdvice{Action: "block", Code: ReasonRepeatedExactBlock, Count: count, Tool: name, Message: exactBlockMessage(name, count)}
				}
				return GuardrailAdvice{Action: "warn", Code: ReasonRepeatedExactWarning, Count: count, Tool: name, Message: exactWarnMessage(name, count)}
			}
		}
		if count := g.sameToolFailures[name]; count >= SameWarnAfter {
			if halt := g.checkNoProgressWindow(name); halt.Code != "" {
				return halt
			}
			return GuardrailAdvice{Action: "warn", Code: ReasonSameToolWarning, Count: count, Tool: name, Message: sameWarnMessage(name, count)}
		}
		if halt := g.checkNoProgressWindow(name); halt.Code != "" {
			return halt
		}
		return GuardrailAdvice{}
	}

	// Success path.
	if fp != "" {
		delete(g.exactFailures, fp)
	}
	if isProgressReset(name) {
		// A successful mutation is a new experiment: clear same-tool ladders.
		clear(g.sameToolFailures)
	} else {
		delete(g.sameToolFailures, name)
	}
	if isIdempotent(name) && fp != "" {
		hash := resultHash(name, result.Content)
		entry := g.noProgress[fp]
		if entry.hash == hash && entry.count > 0 {
			entry.count++
		} else {
			entry = noProgressEntry{hash: hash, count: 1}
		}
		g.noProgress[fp] = entry
		if entry.count >= NoProgressWarnAfter {
			if entry.count >= NoProgressBlockAfter {
				block := Decision{ReasonCode: ReasonIdempotentNoProgressBlock}
				g.setHaltDecision(&block)
				return GuardrailAdvice{Action: "block", Code: ReasonIdempotentNoProgressBlock, Count: entry.count, Tool: name, Message: noProgressBlockMessage(name, entry.count)}
			}
			return GuardrailAdvice{Action: "warn", Code: ReasonIdempotentNoProgressWarn, Count: entry.count, Tool: name, Message: noProgressWarnMessage(name, entry.count)}
		}
	}
	if unproductive {
		if halt := g.checkNoProgressWindow(name); halt.Code != "" {
			return halt
		}
	}
	return GuardrailAdvice{}
}

// checkNoProgressWindow halts when the consecutive no-progress streak fills
// the full window. Callers invoke it on non-productive completions after the
// more specific ladders have been consulted.
func (g *Governor) checkNoProgressWindow(name string) GuardrailAdvice {
	if g.nonProductiveStreak >= NoProgressWindow {
		halt := Decision{ReasonCode: ReasonNoProgressWindowHalt}
		g.setHaltDecision(&halt)
		return GuardrailAdvice{Action: "halt", Code: ReasonNoProgressWindowHalt, Count: g.nonProductiveStreak, Tool: name, Message: noProgressWindowMessage(name, g.nonProductiveStreak)}
	}
	return GuardrailAdvice{}
}

// ObserveCall tracks consecutive identical (tool, args, result) streaks,
// batch cycles, and byte-identical large duplicates. Results are transient.
func (g *Governor) ObserveCall(name string, args map[string]interface{}, content string, callID string, failed bool, fp string) ObserveResult {
	if fp == "" {
		var err error
		fp, err = Fingerprint(name, args, nil)
		if err != nil {
			return ObserveResult{}
		}
	}
	hash := resultHash(name, content)
	repeatable := isRepeatablePoller(name)

	if g.identicalCount > 0 && g.identicalSig == fp && g.identicalHash == hash {
		g.identicalCount++
	} else {
		g.identicalSig = fp
		g.identicalHash = hash
		g.identicalCount = 1
		g.identicalFirstID = callID
	}
	g.history = append(g.history, historyEntry{sig: fp, hash: hash, repeatable: repeatable})
	if len(g.history) > CycleHistory {
		g.history = append([]historyEntry(nil), g.history[len(g.history)-CycleHistory:]...)
	}

	var out ObserveResult
	if !repeatable && !failed && g.identicalCount == IdenticalNoticeAfter {
		out.Notice = fmt.Sprintf("[agent note: this is the 3rd consecutive identical call to %s with identical arguments returning the same result. Do not repeat it \u2014 change arguments, use a different tool, or proceed with what you have.]", name)
		out.Kind = "identical_streak_notice"
	}
	if !repeatable && g.identicalCount >= NoProgressBlockAfter {
		halt := Decision{ReasonCode: ReasonIdenticalStreakHalt}
		g.setHaltDecision(&halt)
		out.Notice = fmt.Sprintf("[Tool hard stop: %s; count=%d; the same call returned the same result %d times in a row. Stop repeating it.]", ReasonIdenticalStreakHalt, g.identicalCount, g.identicalCount)
		out.Kind = ReasonIdenticalStreakHalt
		return out
	}
	if period, laps := detectCycle(g.history); laps >= 3 && period >= CycleMinPeriod && period <= CycleMaxPeriod {
		if !historyHasRepeatable(g.history[len(g.history)-period*laps:]) {
			out.Kind = "identical_cycle_notice"
			out.Notice = fmt.Sprintf("[agent note: the last %d rounds repeated the same cycle of %d tool calls (ending with %s) with identical arguments and identical results. Do not repeat the batch \u2014 change arguments, use a different tool, or proceed with what you have.]", laps, period, name)
			halt := Decision{ReasonCode: ReasonIdenticalCycleHalt}
			g.setHaltDecision(&halt)
			out.Kind = ReasonIdenticalCycleHalt
		}
	}
	if !failed && g.identicalCount >= 2 && utf8.RuneCountInString(content) >= StubMinChars {
		out.Stub = fmt.Sprintf("[agent note: this result is byte-identical to the %s result earlier this turn (tool_call_id %s). Refer to that result; it has not changed. Args: %s]", name, g.identicalFirstID, argsPreview(args))
		if out.Kind == "" {
			out.Kind = "large_duplicate_stub"
		}
	}
	return out
}

// FormatGuardrailWarning renders the single warn-append shape.
func FormatGuardrailWarning(code, tool string, count int, message string) string {
	return fmt.Sprintf("[Tool loop warning: %s; count=%d; %s]", code, count, message)
}

func exactWarnMessage(tool string, count int) string {
	return fmt.Sprintf("%s has failed %d times with identical arguments. This looks like a loop; inspect the error and change strategy instead of retrying it unchanged.", tool, count)
}

func exactBlockMessage(tool string, count int) string {
	return fmt.Sprintf("Blocked %s: the same tool call failed %d times with identical arguments. Stop retrying it unchanged; change strategy or explain the blocker.", tool, count)
}

func sameWarnMessage(tool string, count int) string {
	base := fmt.Sprintf("%s has failed %d times this turn. This looks like a loop. Do not switch to text-only replies; keep using tools, but diagnose before retrying. First inspect the latest error/output and verify your assumptions. Try different arguments, a narrower query/path, an absolute path when relevant, or a different tool that can make progress. If the blocker is external, report the blocker after one diagnostic attempt instead of repeating the same failing path.", tool, count)
	if tool == "web_search" {
		base += " Empty [] is a valid result; check query wording and limit, and try different keywords rather than the same query."
	}
	return base
}

func haltMessage(tool string, count int) string {
	return fmt.Sprintf("Stopped %s: it failed %d times this turn. Stop retrying the same failing tool path and choose a different approach.", tool, count)
}

func noProgressWarnMessage(tool string, count int) string {
	return fmt.Sprintf("%s returned the same result %d times. Use the result already provided or change the query instead of repeating it unchanged.", tool, count)
}

func noProgressBlockMessage(tool string, count int) string {
	return fmt.Sprintf("Stopped %s: the same call with identical arguments returned the same result %d times in a row. Use the earlier result or change strategy.", tool, count)
}

func noProgressWindowMessage(tool string, count int) string {
	return fmt.Sprintf("Stopped %s: the last %d tool calls produced no productive result. Stop repeating unproductive paths; use the results already gathered or explain the blocker.", tool, count)
}

// resultHash digests canonical JSON when parseable, else raw text. A small
// volatile key set is stripped so re-encoded execution metadata does not
// defeat identical-result detection.
func resultHash(tool, content string) string {
	normalized := content
	var decoded interface{}
	if err := json.Unmarshal([]byte(content), &decoded); err == nil {
		stripVolatile(decoded)
		if encoded, err := json.Marshal(decoded); err == nil {
			normalized = string(encoded)
		}
	}
	sum := sha256.Sum256(append(append([]byte(tool), 0), normalized...))
	return hex.EncodeToString(sum[:])
}

func stripVolatile(value interface{}) {
	obj, ok := value.(map[string]interface{})
	if !ok {
		return
	}
	for _, key := range []string{"duration_ms", "durationMs", "execution_count", "executionCount", "elapsed_ms", "elapsedMs"} {
		delete(obj, key)
	}
	for _, nested := range obj {
		switch typed := nested.(type) {
		case map[string]interface{}:
			stripVolatile(typed)
		case []interface{}:
			for _, item := range typed {
				if m, ok := item.(map[string]interface{}); ok {
					stripVolatile(m)
				}
			}
		}
	}
}

// innerRemoteNames extracts deferred remote names from a tool_call bridge
// argument envelope so same-tool ladders attribute to the inner remote.
// Parse errors are ignored: outer tracking still applies.
func innerRemoteNames(args map[string]interface{}) []string {
	if args == nil {
		return nil
	}
	raw, ok := args["entries"]
	if !ok {
		raw, ok = args["calls"]
		if !ok {
			return nil
		}
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	var names []string
	for _, item := range list {
		obj, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := obj["name"].(string)
		name = strings.TrimSpace(name)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func detectCycle(history []historyEntry) (period, laps int) {
	for p := CycleMinPeriod; p <= CycleMaxPeriod; p++ {
		for l := 3; l <= len(history)/p; l++ {
			if l != 3 {
				continue
			}
			base := history[len(history)-p*l:]
			match := true
			for i := p; i < len(base); i++ {
				if base[i].sig != base[i%p].sig || base[i].hash != base[i%p].hash {
					match = false
					break
				}
			}
			if match {
				return p, l
			}
		}
	}
	return 0, 0
}

func historyHasRepeatable(entries []historyEntry) bool {
	for _, e := range entries {
		if e.repeatable {
			return true
		}
	}
	return false
}

func argsPreview(args map[string]interface{}) string {
	if len(args) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return "{unencodable}"
	}
	runes := []rune(string(encoded))
	if len(runes) > StubArgsPreviewRunes {
		return string(runes[:StubArgsPreviewRunes]) + "\u2026"
	}
	return string(runes)
}
