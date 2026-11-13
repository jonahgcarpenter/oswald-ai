package writefile

import (
	"strings"
	"testing"
)

func TestDefinitionContract(t *testing.T) {
	def := Definition()
	if def.Name != Name || def.Name == "" || def.Description == "" {
		t.Fatalf("invalid definition: %+v", def)
	}
	if def.Parameters.Type != "object" || def.Parameters.Properties == nil {
		t.Fatalf("invalid parameters: %+v", def.Parameters)
	}
	if !strings.Contains(def.Description, "patch") {
		t.Fatalf("definition lost the patch redirect: %q", def.Description)
	}
}
