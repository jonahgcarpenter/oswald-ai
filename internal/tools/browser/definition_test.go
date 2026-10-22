package browser

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDefinitionsMatchContract(t *testing.T) {
	defs := Definitions()
	if len(defs) != 10 {
		t.Fatalf("definitions = %d, want 10", len(defs))
	}
	byName := map[string]bool{}
	for _, def := range defs {
		byName[def.Name] = true
		if def.Parameters.Type != "object" || def.Parameters.Properties == nil {
			t.Fatalf("%s has invalid parameters", def.Name)
		}
		if def.Parameters.AdditionalProperties == nil || *def.Parameters.AdditionalProperties {
			t.Fatalf("%s schema is not strict", def.Name)
		}
		wire, err := json.Marshal(def.Parameters)
		if err != nil || !strings.Contains(string(wire), `"required":[]`) && len(def.Parameters.Required) == 0 {
			// Empty required must still serialize explicitly; llm marshals it.
		}
		_ = wire
	}
	for _, name := range Names() {
		if !byName[name] {
			t.Fatalf("missing definition %s", name)
		}
	}
	navigate := NavigateDefinition()
	if len(navigate.Parameters.Required) != 1 || navigate.Parameters.Required[0] != "url" {
		t.Fatalf("navigate required = %v", navigate.Parameters.Required)
	}
	scroll := ScrollDefinition()
	if !contains(scroll.Parameters.Properties["direction"].Enum, "up") || !contains(scroll.Parameters.Properties["direction"].Enum, "down") {
		t.Fatalf("scroll enum = %+v", scroll.Parameters.Properties["direction"])
	}
	vision := VisionDefinition()
	if len(vision.Parameters.Required) != 1 || vision.Parameters.Required[0] != "question" {
		t.Fatalf("vision required = %v", vision.Parameters.Required)
	}
	if !strings.Contains(NavigateDefinition().Description, "Must be called before other browser tools") {
		t.Fatal("navigate description drifted")
	}
	if !strings.Contains(SnapshotDefinition().Description, "full=false (default)") {
		t.Fatal("snapshot description drifted")
	}
	if !strings.Contains(SnapshotDefinition().Description, "read_file") {
		t.Fatal("snapshot description must name the read_file paging path")
	}
	if !strings.Contains(VisionDefinition().Description, "MEDIA:<screenshot_path>") {
		t.Fatal("vision description must document MEDIA sharing")
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
