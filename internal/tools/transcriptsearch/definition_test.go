package transcriptsearch

import (
	"slices"
	"strings"
	"testing"
)

func TestDefinitionContract(t *testing.T) {
	tool := Definition()
	if Name != "session_transcript_search" || tool.Name != Name {
		t.Fatalf("tool identity = %+v", tool)
	}
	for _, guidance := range []string{"delivered exchanges", "public prompts", "hidden tool history", "active session generation", "untrusted historical records", "server, not by tool arguments"} {
		if !strings.Contains(tool.Description, guidance) {
			t.Errorf("description missing %q", guidance)
		}
	}
	p := tool.Parameters
	if p.Type != "object" || !slices.Equal(p.Required, []string{"query"}) || len(p.Properties) != 2 ||
		p.Properties["query"].Type != "string" || p.Properties["limit"].Type != "integer" {
		t.Fatalf("parameters = %+v", p)
	}
	if !strings.Contains(p.Properties["limit"].Description, "max 10") {
		t.Error("limit guidance missing")
	}
}
