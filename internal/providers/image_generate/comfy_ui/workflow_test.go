package comfy_ui

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func workflowPath(name string) string {
	return filepath.Join("..", "..", "..", "..", config.DefaultDataRoot, "workflows", "comfyui", name)
}

func TestBuildMutatesOnlyAllowedWorkflowFields(t *testing.T) {
	for _, test := range []struct {
		mode     Mode
		filename string
	}{
		{mode: TextToImage, filename: "text-to-image-basic.json"},
		{mode: ImageToImage, filename: "image-to-image-basic.json"},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			workflow, err := LoadWorkflow(workflowPath(test.filename), test.mode)
			if err != nil {
				t.Fatal(err)
			}
			expected := cloneNodes(t, workflow.nodes)
			built, seed, _, err := workflow.build("positive", "negative", nil, "landscape")
			if err != nil {
				t.Fatal(err)
			}
			if test.mode == TextToImage {
				expected["5"].Inputs["width"], expected["5"].Inputs["height"] = float64(768), float64(448)
				expected["6"].Inputs["text"] = "positive"
				expected["7"].Inputs["text"] = "negative"
				expected["3"].Inputs["seed"] = seed
			} else {
				expected["32"].Inputs["width"], expected["32"].Inputs["height"] = float64(768), float64(448)
				expected["24"].Inputs["text"] = "positive"
				expected["25"].Inputs["text"] = "negative"
				expected["26"].Inputs["seed"] = seed
				expected["29"].Inputs["image"] = InputImageReference
			}
			if !reflect.DeepEqual(built, expected) {
				t.Fatalf("workflow contained mutations outside the approved fields\nbuilt=%+v\nwant=%+v", built, expected)
			}
			if reflect.DeepEqual(workflow.nodes, built) {
				t.Fatal("immutable template unexpectedly matched the mutated workflow")
			}
		})
	}
}

func cloneNodes(t *testing.T, nodes map[string]node) map[string]node {
	t.Helper()
	data, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]node
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func TestLoadAndBuildWorkflows(t *testing.T) {
	text, err := LoadWorkflow(workflowPath("text-to-image-basic.json"), TextToImage)
	if err != nil {
		t.Fatal(err)
	}
	built, _, _, err := text.build("positive", "negative", nil, "square")
	if err != nil {
		t.Fatal(err)
	}
	if built["6"].Inputs["text"] != "positive" || built["7"].Inputs["text"] != "negative" {
		t.Fatalf("prompts not replaced: %+v", built)
	}
	for key, want := range map[string]interface{}{
		"sampler_name": "dpmpp_2m", "scheduler": "karras",
		"steps": float64(20), "cfg": float64(7), "denoise": float64(1),
	} {
		if got := built["3"].Inputs[key]; got != want {
			t.Errorf("text sampler %s = %v, want %v", key, got, want)
		}
	}
	for key, want := range map[string]interface{}{
		"width": float64(512), "height": float64(512), "batch_size": float64(1),
	} {
		if got := built["5"].Inputs[key]; got != want {
			t.Errorf("text latent %s = %v, want %v", key, got, want)
		}
	}
	seed, ok := built["3"].Inputs["seed"].(uint32)
	if !ok {
		t.Fatalf("seed type = %T, want uint32", built["3"].Inputs["seed"])
	}
	_ = seed
	strength := 0.6
	if _, _, _, err := text.build("positive", "negative", &strength, "square"); err == nil {
		t.Fatal("text-to-image accepted a denoise override")
	}
	if text.nodes["6"].Inputs["text"] == "positive" {
		t.Fatal("template was mutated")
	}

	image, err := LoadWorkflow(workflowPath("image-to-image-basic.json"), ImageToImage)
	if err != nil {
		t.Fatal(err)
	}
	built, _, _, err = image.build("transform", "", nil, "square")
	if err != nil {
		t.Fatal(err)
	}
	if built["29"].Inputs["image"] != InputImageReference || built["26"].Inputs["sampler_name"] != "dpmpp_2m" || built["26"].Inputs["scheduler"] != "karras" {
		t.Fatalf("image workflow contract changed: %+v", built)
	}
}

func TestWorkflowValidationRejectsUnsafeBounds(t *testing.T) {
	w, err := LoadWorkflow(workflowPath("text-to-image-basic.json"), TextToImage)
	if err != nil {
		t.Fatal(err)
	}
	w.nodes["5"].Inputs["width"] = float64(4096)
	if err := w.validate(); err == nil {
		t.Fatal("unsafe dimensions accepted")
	}
}

func TestAspectRatioBuildClonesBothWorkflowModes(t *testing.T) {
	for _, mode := range []Mode{TextToImage, ImageToImage} {
		filename := "text-to-image-basic.json"
		dimensionNode := "5"
		if mode == ImageToImage {
			filename, dimensionNode = "image-to-image-basic.json", "32"
		}
		workflow, err := LoadWorkflow(workflowPath(filename), mode)
		if err != nil {
			t.Fatal(err)
		}
		original := cloneNodes(t, workflow.nodes)
		for _, tc := range []struct {
			aspect        string
			width, height int
		}{
			{"landscape", 768, 448}, {"square", 512, 512}, {"portrait", 448, 768},
		} {
			t.Run(string(mode)+"/"+tc.aspect, func(t *testing.T) {
				built, seed, _, err := workflow.build("positive", "negative", nil, tc.aspect)
				if err != nil {
					t.Fatal(err)
				}
				want := cloneNodes(t, original)
				want[dimensionNode].Inputs["width"], want[dimensionNode].Inputs["height"] = float64(tc.width), float64(tc.height)
				if mode == TextToImage {
					want["6"].Inputs["text"], want["7"].Inputs["text"], want["3"].Inputs["seed"] = "positive", "negative", seed
				} else {
					want["24"].Inputs["text"], want["25"].Inputs["text"], want["26"].Inputs["seed"] = "positive", "negative", seed
					want["29"].Inputs["image"] = InputImageReference
				}
				if !reflect.DeepEqual(built, want) || !reflect.DeepEqual(workflow.nodes, original) {
					t.Fatal("workflow changed beyond approved fields or template mutated")
				}
			})
		}
		for _, aspect := range []string{"", "wide", "LANDSCAPE"} {
			if _, _, _, err := workflow.build("positive", "", nil, aspect); err == nil {
				t.Fatalf("accepted aspect %q", aspect)
			}
		}
	}
}
