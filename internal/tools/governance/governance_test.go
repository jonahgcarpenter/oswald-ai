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
	if policy != (GlobalPolicy{MaxExecutions: 50, MaxToolIterations: 30}) {
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
	g := New(GlobalPolicy{MaxExecutions: 20, MaxToolIterations: 20})
	policy := ToolPolicy{}
	for i := 0; i < 10; i++ {
		decision := g.BeforeExecution("unlimited.tool", map[string]interface{}{"n": i}, policy, true)
		if !decision.Allowed {
			t.Fatalf("call %d blocked: %+v", i, decision)
		}
		g.RecordResult("unlimited.tool", decision, Result{Outcome: OutcomeUnproductive}, assertError{})
	}
	if g.GlobalStopReason() != "" || g.Stats("unlimited.tool").Failures != 10 {
		t.Fatalf("failures must remain statistics, not a ceiling: %+v", g.Stats("unlimited.tool"))
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
