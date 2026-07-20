package runtime

import (
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

func TestExposureTrimsAndDeduplicatesToolNames(t *testing.T) {
	exposure := NewExposure()
	exposure.ExposeTools([]string{" github.get_issue ", "", "github.get_issue"})
	visibility := exposure.Visibility()

	if len(visibility.ExposedMCPTools) != 1 || !visibility.ExposedMCPTools["github.get_issue"] {
		t.Fatalf("unexpected exposed tools: %+v", visibility.ExposedMCPTools)
	}
}

func TestExposureKeepsGlobalEvidenceRequestLocal(t *testing.T) {
	exposure := NewExposure()
	exposure.RecordGlobalToolEvidence(requestctx.GlobalToolEvidence{ToolCallID: "call-1", ServerID: "server", Result: "go 1.24"})
	evidence, ok := exposure.GlobalToolEvidence("call-1")
	if !ok || evidence.ServerID != "server" || evidence.Result != "go 1.24" {
		t.Fatalf("unexpected evidence: %+v ok=%v", evidence, ok)
	}
	if _, ok := NewExposure().GlobalToolEvidence("call-1"); ok {
		t.Fatal("evidence leaked across requests")
	}
}

func TestNilExposureVisibilityIsEmpty(t *testing.T) {
	var exposure *Exposure
	if exposure.Visibility().ExposedMCPTools != nil {
		t.Fatal("expected empty visibility")
	}
	exposure.ExposeTools([]string{"github.get_issue"})
}
