package image_generate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDefinition(t *testing.T) {
	d := Definition()
	if d.Name != Name || Name != "image_generate" || d.Parameters.Type != "object" || !reflect.DeepEqual(d.Parameters.Required, []string{"prompt"}) || d.Parameters.AdditionalProperties == nil || *d.Parameters.AdditionalProperties || len(d.Parameters.Properties) != 3 {
		t.Fatalf("definition: %+v", d)
	}
	if !reflect.DeepEqual(d.Parameters.Properties["aspect_ratio"].Enum, []string{"landscape", "square", "portrait"}) || d.Parameters.Properties["aspect_ratio"].Default != "landscape" || d.Parameters.Properties["image_url"].Type != "string" {
		t.Fatal("aspect/source schema incorrect")
	}
	wire, err := json.Marshal(d.Parameters)
	if err != nil || !strings.Contains(string(wire), `"default":"landscape"`) {
		t.Fatalf("aspect default missing from wire schema: %s, %v", wire, err)
	}
	for _, text := range []string{"attached", "server-owned", "source image IDs", "approximate", "default"} {
		if !strings.Contains(d.Description, text) {
			t.Errorf("description missing %q", text)
		}
	}
}
