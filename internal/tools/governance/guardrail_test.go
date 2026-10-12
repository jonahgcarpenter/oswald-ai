package governance

import (
	"errors"
	"strings"
	"testing"
)

func TestAfterCallExactWarn(t *testing.T) {
	g := New(testGlobal())
	args := map[string]interface{}{"q": "same"}
	fp, err := Fingerprint("memory", args, nil)
	if err != nil {
		t.Fatal(err)
	}
	var advice GuardrailAdvice
	for i := 0; i < 2; i++ {
		advice = g.AfterCall("memory", args, fp, Result{}, errors.New("boom"))
	}
	if !advice.Warn() || advice.Code != ReasonRepeatedExactWarning || advice.Count != 2 {
		t.Fatalf("advice = %+v", advice)
	}
	if text := FormatGuardrailWarning(advice.Code, advice.Tool, advice.Count, advice.Message); !strings.Contains(text, "count=2") {
		t.Fatalf("warn text = %q", text)
	}
}

func TestAfterCallSameWarnAndProgressReset(t *testing.T) {
	g := New(testGlobal())
	for i := 0; i < 3; i++ {
		fp, _ := Fingerprint("session_search", map[string]interface{}{"n": i}, nil)
		advice := g.AfterCall("session_search", map[string]interface{}{"n": i}, fp, Result{}, errors.New("boom"))
		if i < 2 && advice.Warn() {
			t.Fatalf("early warn at %d: %+v", i, advice)
		}
		if i == 2 && (!advice.Warn() || advice.Code != ReasonSameToolWarning || advice.Count != 3) {
			t.Fatalf("advice = %+v", advice)
		}
	}
	// Successful memory mutation clears same-tool ladders globally.
	fp, _ := Fingerprint("memory", map[string]interface{}{"op": "add"}, nil)
	g.AfterCall("memory", map[string]interface{}{"op": "add"}, fp, Result{Outcome: OutcomeProductive}, nil)
	if count := g.SameToolFailures("session_search"); count != 0 {
		t.Fatalf("same failures not reset: %d", count)
	}
}

func TestAfterCallDegradedNotFailure(t *testing.T) {
	g := New(testGlobal())
	fp, _ := Fingerprint("web_search", map[string]interface{}{"q": "x"}, nil)
	advice := g.AfterCall("web_search", map[string]interface{}{"q": "x"}, fp, Result{Outcome: OutcomeProductive, IsDegraded: true}, nil)
	if advice.Warn() {
		t.Fatalf("degraded advice = %+v", advice)
	}
	if stats := g.Stats("web_search"); stats.Failures != 0 || stats.Productive != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestAfterCallUnproductiveStillRetires(t *testing.T) {
	g := New(GlobalPolicy{MaxExecutions: 50, MaxToolIterations: 50})
	policy := ToolPolicy{MaxUnproductive: 2, BlockDuplicates: true}
	for i := 0; i < 2; i++ {
		args := map[string]interface{}{"n": i}
		d := g.BeforeExecution("session_search", args, policy, true)
		if !d.Allowed {
			t.Fatalf("call %d blocked: %+v", i, d)
		}
		g.AfterCall("session_search", args, d.Fingerprint(), Result{Outcome: OutcomeUnproductive}, nil)
	}
	if !g.IsToolRetired("session_search", policy) {
		t.Fatal("unproductive tool was not retired")
	}
}

func TestObserveStreakNoticeAndStub(t *testing.T) {
	g := New(testGlobal())
	args := map[string]interface{}{"q": "same"}
	fp, _ := Fingerprint("web_search", args, nil)
	content := strings.Repeat("x", 600)
	var out ObserveResult
	for i := 0; i < 3; i++ {
		out = g.ObserveCall("web_search", args, content, "call-1", false, fp)
	}
	if !strings.Contains(out.Notice, "3rd consecutive identical call") {
		t.Fatalf("notice = %q", out.Notice)
	}
	if out.Stub == "" || !strings.Contains(out.Stub, "byte-identical") {
		t.Fatalf("stub = %q", out.Stub)
	}
	// Pollers are exempt from streak notices.
	g = New(testGlobal())
	fp, _ = Fingerprint("job_poll", args, nil)
	for i := 0; i < 3; i++ {
		out = g.ObserveCall("job_poll", args, content, "call-1", false, fp)
	}
	if out.Notice != "" {
		t.Fatalf("poller notice = %q", out.Notice)
	}
}

func TestObserveCycleNotice(t *testing.T) {
	g := New(testGlobal())
	aArgs := map[string]interface{}{"q": "a"}
	bArgs := map[string]interface{}{"q": "b"}
	aFp, _ := Fingerprint("web_search", aArgs, nil)
	bFp, _ := Fingerprint("web_search", bArgs, nil)
	var out ObserveResult
	for i := 0; i < 3; i++ {
		g.ObserveCall("web_search", aArgs, "result-a", "id-a", false, aFp)
		out = g.ObserveCall("web_search", bArgs, "result-b", "id-b", false, bFp)
	}
	if !strings.Contains(out.Notice, "same cycle of 2 tool calls") {
		t.Fatalf("cycle notice = %q", out.Notice)
	}
}

func TestHardStopExactBlock(t *testing.T) {
	g := New(GlobalPolicy{MaxExecutions: 50, MaxToolIterations: 50})
	g.SetHardStopEnabled(true)
	policy := ToolPolicy{BlockDuplicates: true}
	args := map[string]interface{}{"q": "same"}
	for i := 0; i < 5; i++ {
		d := g.BeforeExecution("memory", args, policy, true)
		if !d.Allowed {
			t.Fatalf("call %d blocked early: %+v", i, d)
		}
		g.AfterCall("memory", args, d.Fingerprint(), Result{}, errors.New("boom"))
	}
	blocked := g.BeforeExecution("memory", args, policy, true)
	if blocked.Allowed || blocked.ReasonCode != ReasonRepeatedExactBlock {
		t.Fatalf("blocked = %+v", blocked)
	}
	if g.HaltDecision() == nil {
		t.Fatal("halt decision not recorded")
	}
}
