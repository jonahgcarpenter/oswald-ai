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
		"description":"Generate high-quality images from text prompts, or edit / transform an existing image by passing image_url. Returns the result in the ` + "`" + `image` + "`" + ` field — a URL or an absolute file path; reference it in your response using the current platform's file-delivery convention.",
		"parameters":{"type":"object","properties":{
			"prompt":{"type":"string","description":"The text prompt describing the desired image (text-to-image) or the edit to apply (image-to-image). Be detailed and descriptive."},
			"aspect_ratio":{"type":"string","description":"The aspect ratio of the generated image. 'landscape' is 16:9 wide, 'portrait' is 16:9 tall, 'square' is 1:1.","enum":["landscape","square","portrait"],"default":"landscape"},
			"image_url":{"type":"string","description":"Source image to edit/transform (image-to-image). A public URL or an absolute local file path from the conversation. Omit for text-to-image."}
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
