package tools

import (
	"testing"
)

func TestToolsetMembers(t *testing.T) {
	if len(ToolsetMembers(ToolsetCore)) == 0 || len(ToolsetMembers(ToolsetTerminal)) != 2 {
		t.Fatal("toolset membership wrong")
	}
	if ToolsetMembers("unknown") != nil {
		t.Fatal("unknown toolset should be nil")
	}
	if sets := GatewayToolsets("openai"); len(sets) != 1 || sets[0] != ToolsetCore {
		t.Fatalf("openai toolsets = %v", sets)
	}
	if sets := GatewayToolsets("discord"); len(sets) != 4 {
		t.Fatalf("discord toolsets = %v", sets)
	}
}
