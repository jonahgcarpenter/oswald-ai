package image_generate

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

func TestDefinition(t *testing.T) {
	fixture := []byte(`{
		"name":"image_generate",
		"description":"Generate an image from a text description, or transform an existing image by supplying image_url. For transformations, describe the complete desired final image and the details to retain. Editing affects the whole image and may change faces, geometry, or other details; exact preservation and localized edits are not guaranteed. Returns the image directly in the tool result with its private absolute cache path and source dimensions. The model preview may be downscaled; the latest successful original version is delivered as an attachment with the final response.",
		"parameters":{"type":"object","properties":{
			"prompt":{"type":"string","description":"Describe the desired final image, including subject, composition, style, lighting, and colors. When editing, clearly describe the change and the existing details to retain.","required":[]},
			"aspect_ratio":{"type":"string","description":"Output aspect ratio: landscape is 1280x720, square is 1024x1024, and portrait is 720x1280. When editing, match the source ratio where possible to avoid center cropping.","enum":["landscape","square","portrait"],"default":"landscape","required":[]},
			"image_url":{"type":"string","description":"Optional source image: a public HTTPS image URL or an absolute path to an unexpired image in the current user's managed image cache. Omit to generate a new image from text.","required":[]}
		},"required":["prompt"]}
	}`)
	assertJSON := func(label string, actual, expected []byte) {
		t.Helper()
		var got, want interface{}
		if err := json.Unmarshal(actual, &got); err != nil {
			t.Fatalf("%s decode actual: %v", label, err)
		}
		if err := json.Unmarshal(expected, &want); err != nil {
			t.Fatalf("%s decode fixture: %v", label, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s mismatch: got %s, want %s", label, actual, expected)
		}
	}
	definition := Definition()
	encoded, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	assertJSON("definition", encoded, fixture)
	wire, err := json.Marshal(llm.Tool{Type: "function", Function: definition})
	if err != nil {
		t.Fatal(err)
	}
	assertJSON("model wire", wire, append(append([]byte(`{"type":"function","function":`), fixture...), '}'))
}
