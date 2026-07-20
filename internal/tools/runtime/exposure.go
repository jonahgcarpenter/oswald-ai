package runtime

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Exposure tracks tools exposed during a single agent request.
type Exposure struct {
	mcpTools       map[string]bool
	globalEvidence map[string]requestctx.GlobalToolEvidence
}

// NewExposure creates empty request-local tool exposure state.
func NewExposure() *Exposure {
	return &Exposure{
		mcpTools:       make(map[string]bool),
		globalEvidence: make(map[string]requestctx.GlobalToolEvidence),
	}
}

// RecordGlobalToolEvidence records a successful global MCP result.
func (e *Exposure) RecordGlobalToolEvidence(evidence requestctx.GlobalToolEvidence) {
	if e == nil || strings.TrimSpace(evidence.ToolCallID) == "" {
		return
	}
	e.globalEvidence[evidence.ToolCallID] = evidence
}

// GlobalToolEvidence returns same-request evidence by exact tool-call ID.
func (e *Exposure) GlobalToolEvidence(toolCallID string) (requestctx.GlobalToolEvidence, bool) {
	if e == nil {
		return requestctx.GlobalToolEvidence{}, false
	}
	evidence, ok := e.globalEvidence[strings.TrimSpace(toolCallID)]
	return evidence, ok
}

// ExposeTools records MCP tools that should be visible for the active request.
func (e *Exposure) ExposeTools(names []string) {
	if e == nil {
		return
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		e.mcpTools[name] = true
	}
}

// Visibility returns the registry visibility represented by this exposure state.
func (e *Exposure) Visibility() registry.ToolVisibility {
	if e == nil {
		return registry.ToolVisibility{}
	}
	return registry.ToolVisibility{ExposedMCPTools: e.mcpTools}
}

// ExposedMCPTools returns a copy of request-local exposed MCP tool names.
func (e *Exposure) ExposedMCPTools() map[string]bool {
	if e == nil {
		return nil
	}
	out := make(map[string]bool, len(e.mcpTools))
	for name, exposed := range e.mcpTools {
		out[name] = exposed
	}
	return out
}
