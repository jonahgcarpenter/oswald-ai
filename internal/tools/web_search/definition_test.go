package web_search

import (
	"slices"
	"strings"
	"testing"
)

func TestDefinitionContract(t *testing.T) {
	definition := Definition()
	if Name != "web_search" || definition.Name != Name {
		t.Fatalf("name = %q, definition name = %q", Name, definition.Name)
	}
	for _, guidance := range []string{"titles, URLs, and descriptions", "site:domain", "backend supports them"} {
		if !strings.Contains(definition.Description, guidance) {
			t.Errorf("description missing %q", guidance)
		}
	}
	p := definition.Parameters
	if p.Type != "object" || !slices.Equal(p.Required, []string{"query"}) || len(p.Properties) != 2 ||
		p.AdditionalProperties == nil || *p.AdditionalProperties {
		t.Fatalf("parameters = %+v", p)
	}
	if p.Properties["query"].Type != "string" || !strings.Contains(p.Properties["query"].Description, "site:example.com") {
		t.Error("query contract changed")
	}
	limit := p.Properties["limit"]
	if limit.Type != "integer" || limit.Default != 5 || limit.Minimum == nil || *limit.Minimum != 1 || limit.Maximum == nil || *limit.Maximum != 100 {
		t.Fatalf("limit contract = %+v", limit)
	}
}
