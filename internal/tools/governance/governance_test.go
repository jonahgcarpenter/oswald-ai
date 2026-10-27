package governance

import "testing"

func testPolicy() ToolPolicy {
	return ToolPolicy{}
}

func testGlobal() GlobalPolicy {
	return GlobalPolicy{MaxExecutions: 4, MaxToolIterations: 2}
}

func TestDefaultGlobalPolicy(t *testing.T) {
	policy := DefaultGlobalPolicy()
	if policy != (GlobalPolicy{MaxExecutions: 500, MaxToolIterations: 200}) {
		t.Fatalf("default policy = %+v", policy)
	}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintCanonicalizesMapOrder(t *testing.T) {
	a, err := Fingerprint("test.tool", map[string]interface{}{"a": 1, "b": "two"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint("test.tool", map[string]interface{}{"b": "two", "a": 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("fingerprints differ: %q != %q", a, b)
	}
}

func TestPoliciesValidateSafetyLimits(t *testing.T) {
	if err := testPolicy().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := testGlobal().Validate(); err != nil {
		t.Fatal(err)
	}
	invalidHistory := testPolicy()
	invalidHistory.History = HistoryPolicy{Mode: "unknown"}
	if err := invalidHistory.Validate(); err == nil {
		t.Fatal("unknown history mode was accepted")
	}
	invalidGlobal := testGlobal()
	invalidGlobal.MaxExecutions = 0
	if err := invalidGlobal.Validate(); err == nil {
		t.Fatal("zero global execution limit was accepted")
	}
}

func TestGovernorExecutesRepeatsWithoutBlocking(t *testing.T) {
	g := New(testGlobal())
	policy := testPolicy()
	for i := 0; i < 3; i++ {
		decision := g.BeforeExecution("test.tool", map[string]interface{}{"q": "one"}, policy, true)
		if !decision.Allowed {
			t.Fatalf("repeat %d blocked: %+v", i, decision)
		}
		g.RecordResult("test.tool", decision, Result{Outcome: OutcomeProductive}, nil)
	}
	stats := g.Stats("test.tool")
	if stats.Attempts != 3 || stats.Executions != 3 || g.TotalExecutions() != 3 {
		t.Fatalf("unexpected stats: %+v total=%d", stats, g.TotalExecutions())
	}
}

func TestGovernorAllowsExactRetryAfterExecutionFailure(t *testing.T) {
	g := New(testGlobal())
	policy := testPolicy()
	args := map[string]interface{}{"q": "same"}

	first := g.BeforeExecution("test.tool", args, policy, true)
	if !first.Allowed {
		t.Fatalf("first decision = %+v", first)
	}
	g.RecordResult("test.tool", first, Result{}, assertError{})

	retry := g.BeforeExecution("test.tool", args, policy, true)
	if !retry.Allowed {
		t.Fatalf("retry decision = %+v", retry)
	}
	g.RecordResult("test.tool", retry, Result{Outcome: OutcomeProductive}, nil)

	third := g.BeforeExecution("test.tool", args, policy, true)
	if !third.Allowed {
		t.Fatalf("post-success repeat decision = %+v", third)
	}
}

func TestGovernorNeverRetiresToolsOnFailures(t *testing.T) {
	// Failure-tolerant bridge tools never trip the same-tool halt ladder:
	// failures remain statistics, not a ceiling.
	g := New(GlobalPolicy{MaxExecutions: 20, MaxToolIterations: 20})
	policy := ToolPolicy{}
	for i := 0; i < 10; i++ {
		decision := g.BeforeExecution("mcp__remote", map[string]interface{}{"n": i}, policy, true)
		if !decision.Allowed {
			t.Fatalf("call %d blocked: %+v", i, decision)
		}
		g.RecordResult("mcp__remote", decision, Result{Outcome: OutcomeUnproductive}, assertError{})
	}
	if g.HaltDecision() != nil {
		t.Fatalf("tolerant tool halted: %+v", g.HaltDecision())
	}
}

func TestGovernorHaltsSameToolFailureRun(t *testing.T) {
	g := New(GlobalPolicy{MaxExecutions: 50, MaxToolIterations: 50})
	policy := ToolPolicy{}
	var advice GuardrailAdvice
	for i := 0; i < SameHaltAfter; i++ {
		args := map[string]interface{}{"n": i}
		d := g.BeforeExecution("unlimited.tool", args, policy, true)
		if !d.Allowed {
			t.Fatalf("call %d blocked: %+v", i, d)
		}
		advice = g.AfterCall("unlimited.tool", args, d.Fingerprint(), Result{}, assertError{})
	}
	if advice.Action != "halt" || advice.Code != ReasonSameToolHalt {
		t.Fatalf("advice = %+v, want same-tool halt", advice)
	}
	if reason := g.GlobalStopReason(); reason != ReasonSameToolHalt {
		t.Fatalf("stop reason = %q, want %q", reason, ReasonSameToolHalt)
	}
}

func TestGovernorHaltsNoProgressWindow(t *testing.T) {
	g := New(GlobalPolicy{MaxExecutions: 100, MaxToolIterations: 100})
	policy := ToolPolicy{}
	var advice GuardrailAdvice
	// Vary tool and args so no single-signature ladder trips first.
	tools := []string{"alpha.tool", "beta.tool", "gamma.tool", "delta.tool"}
	for i := 0; i < NoProgressWindow; i++ {
		name := tools[i%len(tools)]
		args := map[string]interface{}{"n": i}
		d := g.BeforeExecution(name, args, policy, true)
		if !d.Allowed {
			t.Fatalf("call %d blocked: %+v", i, d)
		}
		advice = g.AfterCall(name, args, d.Fingerprint(), Result{Outcome: OutcomeUnproductive}, nil)
	}
	if advice.Action != "halt" || advice.Code != ReasonNoProgressWindowHalt {
		t.Fatalf("advice = %+v, want window halt", advice)
	}
	if g.NonProductiveStreak() != NoProgressWindow {
		t.Fatalf("streak = %d, want %d", g.NonProductiveStreak(), NoProgressWindow)
	}
	if reason := g.GlobalStopReason(); reason != ReasonNoProgressWindowHalt {
		t.Fatalf("stop reason = %q, want window halt", reason)
	}
}

func TestGovernorProductiveResetsNoProgressWindow(t *testing.T) {
	g := New(GlobalPolicy{MaxExecutions: 100, MaxToolIterations: 100})
	policy := ToolPolicy{}
	for i := 0; i < NoProgressWindow-1; i++ {
		args := map[string]interface{}{"n": i}
		d := g.BeforeExecution("alpha.tool", args, policy, true)
		if !d.Allowed {
			t.Fatalf("call %d blocked: %+v", i, d)
		}
		g.AfterCall("alpha.tool", args, d.Fingerprint(), Result{Outcome: OutcomeUnproductive}, nil)
	}
	d := g.BeforeExecution("alpha.tool", map[string]interface{}{"n": "good"}, policy, true)
	if !d.Allowed {
		t.Fatalf("productive call blocked: %+v", d)
	}
	g.AfterCall("alpha.tool", map[string]interface{}{"n": "good"}, d.Fingerprint(), Result{Outcome: OutcomeProductive}, nil)
	if g.NonProductiveStreak() != 0 {
		t.Fatalf("streak = %d, want 0 after productive result", g.NonProductiveStreak())
	}
	if reason := g.GlobalStopReason(); reason != "" {
		t.Fatalf("productive reset should clear the window, got %q", reason)
	}
}

func TestGovernorLongProductiveRunPassesOldCeiling(t *testing.T) {
	g := New(DefaultGlobalPolicy())
	policy := ToolPolicy{}
	for i := 0; i < 60; i++ {
		args := map[string]interface{}{"n": i}
		d := g.BeforeExecution("work.tool", args, policy, true)
		if !d.Allowed {
			t.Fatalf("productive call %d blocked: %+v", i, d)
		}
		g.AfterCall("work.tool", args, d.Fingerprint(), Result{Outcome: OutcomeProductive, Content: "ok"}, nil)
	}
	if reason := g.GlobalStopReason(); reason != "" {
		t.Fatalf("productive run halted: %q", reason)
	}
	if g.TotalExecutions() != 60 {
		t.Fatalf("executions = %d, want 60", g.TotalExecutions())
	}
}

func TestGovernorNeverRetiresToolsOnUnproductiveResults(t *testing.T) {
	g := New(GlobalPolicy{MaxExecutions: 20, MaxToolIterations: 20})
	policy := testPolicy()
	for i := 0; i < 4; i++ {
		decision := g.BeforeExecution("empty.tool", map[string]interface{}{"n": i}, policy, true)
		if !decision.Allowed {
			t.Fatalf("call %d blocked: %+v", i, decision)
		}
		g.RecordResult("empty.tool", decision, Result{Outcome: OutcomeUnproductive}, nil)
	}
	if g.GlobalStopReason() != "" {
		t.Fatal("per-tool unproductive results stopped the request")
	}
	goodDecision := g.BeforeExecution("good.tool", map[string]interface{}{"n": 1}, policy, true)
	if !goodDecision.Allowed {
		t.Fatal(goodDecision)
	}
}

func TestGovernorEnforcesGlobalExecutionAndIterationLimits(t *testing.T) {
	global := testGlobal()
	global.MaxExecutions = 1
	g := New(global)
	policy := testPolicy()
	if decision := g.BeginToolIteration(); !decision.Allowed {
		t.Fatal(decision)
	}
	if decision := g.BeforeExecution("one", nil, policy, true); !decision.Allowed {
		t.Fatal(decision)
	}
	if reason := g.GlobalStopReason(); reason != ReasonGlobalLimit {
		t.Fatalf("global stop reason = %q", reason)
	}
	if decision := g.BeforeExecution("two", nil, policy, true); decision.Allowed || decision.ReasonCode != ReasonGlobalLimit {
		t.Fatalf("post-limit decision = %+v", decision)
	}

	g = New(testGlobal())
	if decision := g.BeginToolIteration(); !decision.Allowed {
		t.Fatal(decision)
	}
	if decision := g.BeginToolIteration(); !decision.Allowed {
		t.Fatal(decision)
	}
	if decision := g.BeginToolIteration(); decision.Allowed || decision.ReasonCode != ReasonIterationLimit {
		t.Fatalf("iteration decision = %+v", decision)
	}
}

type assertError struct{}

func (assertError) Error() string { return "failure" }
