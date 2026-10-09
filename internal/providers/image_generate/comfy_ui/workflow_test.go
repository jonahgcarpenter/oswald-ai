package comfy_ui

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func TestBuildMutatesOnlyAllowedWorkflowFields(t *testing.T) {
	for _, test := range []struct {
		mode Mode
	}{
		{mode: TextToImage},
		{mode: ImageToImage},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			workflow, err := NewWorkflow(test.mode)
			if err != nil {
				t.Fatal(err)
			}
			expected := cloneNodes(t, workflow.nodes)
			built, seed, _, err := workflow.build("positive", "negative", nil, "landscape")
			if err != nil {
				t.Fatal(err)
			}
			if test.mode == TextToImage {
				expected["7"].Inputs["width"], expected["7"].Inputs["height"] = float64(1280), float64(720)
				expected["4"].Inputs["text"] = "positive"
				expected["5"].Inputs["text"] = "negative"
				expected["8"].Inputs["seed"] = seed
			} else {
				expected["12"].Inputs["width"], expected["12"].Inputs["height"] = float64(1280), float64(720)
				expected["6"].Inputs["text"] = "positive"
				expected["7"].Inputs["text"] = "negative"
				expected["9"].Inputs["seed"] = seed
				expected["4"].Inputs["image"] = InputImageReference
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

func TestBuildWorkflows(t *testing.T) {
	text, err := NewWorkflow(TextToImage)
	if err != nil {
		t.Fatal(err)
	}
	built, _, _, err := text.build("positive", "negative", nil, "square")
	if err != nil {
		t.Fatal(err)
	}
	if built["4"].Inputs["text"] != "positive" || built["5"].Inputs["text"] != "negative" {
		t.Fatalf("prompts not replaced: %+v", built)
	}
	for key, want := range map[string]interface{}{
		"sampler_name": "euler", "scheduler": "sgm_uniform",
		"steps": float64(4), "cfg": float64(1), "denoise": float64(1),
	} {
		if got := built["8"].Inputs[key]; got != want {
			t.Errorf("text sampler %s = %v, want %v", key, got, want)
		}
	}
	for key, want := range map[string]interface{}{
		"width": float64(1024), "height": float64(1024), "batch_size": float64(1),
	} {
		if got := built["7"].Inputs[key]; got != want {
			t.Errorf("text latent %s = %v, want %v", key, got, want)
		}
	}
	seed, ok := built["8"].Inputs["seed"].(uint32)
	if !ok {
		t.Fatalf("seed type = %T, want uint32", built["8"].Inputs["seed"])
	}
	_ = seed
	strength := 0.6
	if _, _, _, err := text.build("positive", "negative", &strength, "square"); err == nil {
		t.Fatal("text-to-image accepted a denoise override")
	}
	if text.nodes["4"].Inputs["text"] == "positive" {
		t.Fatal("template was mutated")
	}

	image, err := NewWorkflow(ImageToImage)
	if err != nil {
		t.Fatal(err)
	}
	built, _, effectiveStrength, err := image.build("transform", "", nil, "square")
	if err != nil {
		t.Fatal(err)
	}
	if built["4"].Inputs["image"] != InputImageReference || built["9"].Inputs["sampler_name"] != "euler" || built["9"].Inputs["scheduler"] != "sgm_uniform" || effectiveStrength == nil || *effectiveStrength != 0.75 {
		t.Fatalf("image workflow contract changed: %+v", built)
	}
}

func TestWorkflowValidationRejectsUnsafeBounds(t *testing.T) {
	w, err := NewWorkflow(TextToImage)
	if err != nil {
		t.Fatal(err)
	}
	w.nodes["7"].Inputs["width"] = float64(4096)
	if err := w.validate(); err == nil {
		t.Fatal("unsafe dimensions accepted")
	}
	for _, size := range []float64{433, 1296, 1280.5, math.NaN(), math.Inf(1)} {
		w.nodes["7"].Inputs["width"] = size
		if err := w.validate(); err == nil {
			t.Fatalf("unsafe width %v accepted", size)
		}
	}
	w.nodes["7"].Inputs["width"] = float64(1280)
	w.nodes["7"].Inputs["height"] = float64(720)
	if err := w.validate(); err != nil {
		t.Fatalf("exact 16:9 rejected: %v", err)
	}
	w.nodes["7"].Inputs["height"] = float64(1280)
	if err := w.validate(); err == nil {
		t.Fatal("excess pixel count accepted")
	}
}

func TestNewWorkflowModelsAndMode(t *testing.T) {
	if _, err := NewWorkflow("invalid"); err == nil {
		t.Fatal("accepted invalid mode")
	}
	for _, mode := range []Mode{TextToImage, ImageToImage} {
		w, err := NewWorkflow(mode)
		if err != nil {
			t.Fatal(err)
		}
		for _, check := range []struct{ id, key, want string }{
			{"1", "unet_name", "sd3.5_large_turbo-Q5_0.gguf"},
			{"2", "clip_name1", "clip_l.safetensors"},
			{"2", "clip_name2", "t5-v1_1-xxl-encoder-Q5_K_M.gguf"},
			{"3", "vae_name", "diffusion_pytorch_model.safetensors"},
		} {
			if got := w.nodes[check.id].Inputs[check.key]; got != check.want {
				t.Errorf("node %s %s = %v, want %s", check.id, check.key, got, check.want)
			}
		}
	}
}

func TestAspectRatioBuildClonesBothWorkflowModes(t *testing.T) {
	for _, mode := range []Mode{TextToImage, ImageToImage} {
		dimensionNode := "7"
		if mode == ImageToImage {
			dimensionNode = "12"
		}
		workflow, err := NewWorkflow(mode)
		if err != nil {
			t.Fatal(err)
		}
		original := cloneNodes(t, workflow.nodes)
		for _, tc := range []struct {
			aspect        string
			width, height int
		}{
			{"landscape", 1280, 720}, {"square", 1024, 1024}, {"portrait", 720, 1280},
		} {
			t.Run(string(mode)+"/"+tc.aspect, func(t *testing.T) {
				built, seed, _, err := workflow.build("positive", "negative", nil, tc.aspect)
				if err != nil {
					t.Fatal(err)
				}
				want := cloneNodes(t, original)
				want[dimensionNode].Inputs["width"], want[dimensionNode].Inputs["height"] = float64(tc.width), float64(tc.height)
				if mode == TextToImage {
					want["4"].Inputs["text"], want["5"].Inputs["text"], want["8"].Inputs["seed"] = "positive", "negative", seed
				} else {
					want["6"].Inputs["text"], want["7"].Inputs["text"], want["9"].Inputs["seed"] = "positive", "negative", seed
					want["4"].Inputs["image"] = InputImageReference
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

func TestWorkflowValidationRejectsChangedModelsSamplingAndConnections(t *testing.T) {
	for _, mode := range []Mode{TextToImage, ImageToImage} {
		positive, sampling, sampler := "4", "6", "8"
		if mode == ImageToImage {
			positive, sampling, sampler = "6", "8", "9"
		}
		for _, tc := range []struct {
			name, id, key string
			value         interface{}
		}{
			{"model", "1", "unet_name", "different.gguf"},
			{"encoder", "2", "clip_name2", "different.gguf"},
			{"vae", "3", "vae_name", "different.safetensors"},
			{"shift", sampling, "shift", float64(1)},
			{"steps", sampler, "steps", float64(20)},
			{"cfg", sampler, "cfg", float64(7)},
			{"sampler", sampler, "sampler_name", "dpmpp_2m"},
			{"scheduler", sampler, "scheduler", "karras"},
			{"conditioning", positive, "clip", []interface{}{"1", float64(0)}},
			{"model connection", sampler, "model", []interface{}{"1", float64(0)}},
			{"denoise", sampler, "denoise", float64(0)},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				workflow, err := NewWorkflow(mode)
				if err != nil {
					t.Fatal(err)
				}
				workflow.nodes[tc.id].Inputs[tc.key] = tc.value
				if err := workflow.validate(); err == nil {
					t.Fatal("invalid workflow accepted")
				}
			})
		}
	}
}
