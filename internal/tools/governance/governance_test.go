package governance

import "testing"

func testPolicy() ToolPolicy {
	return ToolPolicy{MaxExecutions: 3, MaxFailures: 2, MaxUnproductive: 2, BlockDuplicates: true}
}

func testGlobal() GlobalPolicy {
	return GlobalPolicy{MaxExecutions: 4, MaxToolIterations: 2, MaxConsecutiveFailures: 2}
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
	invalidTool := testPolicy()
	invalidTool.MaxUnproductive = 0
	if err := invalidTool.Validate(); err == nil {
		t.Fatal("zero unproductive limit was accepted")
	}
	invalidGlobal := testGlobal()
	invalidGlobal.MaxExecutions = 0
	if err := invalidGlobal.Validate(); err == nil {
		t.Fatal("zero global execution limit was accepted")
	}
}

func TestGovernorBlocksDuplicatesWithoutConsumingExecution(t *testing.T) {
	g := New(testGlobal())
	policy := testPolicy()
	if decision := g.BeforeExecution("test.tool", map[string]interface{}{"q": "one"}, policy, true); !decision.Allowed {
		t.Fatalf("first decision = %+v", decision)
	}
	g.RecordResult("test.tool", Result{Outcome: OutcomeProductive}, nil)
	decision := g.BeforeExecution("test.tool", map[string]interface{}{"q": "one"}, policy, true)
	if decision.Allowed || decision.ReasonCode != ReasonDuplicate {
		t.Fatalf("duplicate decision = %+v", decision)
	}
	stats := g.Stats("test.tool")
	if stats.Attempts != 2 || stats.Executions != 1 || stats.Duplicates != 1 || g.TotalExecutions() != 1 {
		t.Fatalf("unexpected stats: %+v total=%d", stats, g.TotalExecutions())
	}
}

func TestGovernorRetiresOnlyExhaustedTool(t *testing.T) {
	g := New(testGlobal())
	policy := testPolicy()
	for i := 0; i < policy.MaxUnproductive; i++ {
		if decision := g.BeforeExecution("empty.tool", map[string]interface{}{"n": i}, policy, true); !decision.Allowed {
			t.Fatalf("call %d blocked: %+v", i, decision)
		}
		g.RecordResult("empty.tool", Result{Outcome: OutcomeUnproductive}, nil)
	}
	if !g.IsToolRetired("empty.tool", policy) {
		t.Fatal("empty tool was not retired")
	}
	if g.IsToolRetired("other.tool", policy) {
		t.Fatal("unrelated tool was retired")
	}
}

func TestGovernorProductiveResultResetsOnlyGlobalFailureStreak(t *testing.T) {
	g := New(testGlobal())
	policy := testPolicy()
	if decision := g.BeforeExecution("bad.tool", map[string]interface{}{"n": 1}, policy, true); !decision.Allowed {
		t.Fatal(decision)
	}
	g.RecordResult("bad.tool", Result{}, assertError{})
	if decision := g.BeforeExecution("empty.tool", map[string]interface{}{"n": 1}, policy, true); !decision.Allowed {
		t.Fatal(decision)
	}
	g.RecordResult("empty.tool", Result{Outcome: OutcomeUnproductive}, nil)
	if g.ConsecutiveFailures() != 1 {
		t.Fatalf("unproductive result reset failure streak: %d", g.ConsecutiveFailures())
	}
	if decision := g.BeforeExecution("good.tool", map[string]interface{}{"n": 1}, policy, true); !decision.Allowed {
		t.Fatal(decision)
	}
	g.RecordResult("good.tool", Result{Outcome: OutcomeProductive}, nil)
	if g.ConsecutiveFailures() != 0 {
		t.Fatalf("productive result did not reset failure streak: %d", g.ConsecutiveFailures())
	}
	if g.Stats("bad.tool").Failures != 1 {
		t.Fatal("per-tool failure count was reset")
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
