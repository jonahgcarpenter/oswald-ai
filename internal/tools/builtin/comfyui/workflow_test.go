package comfyui

import (
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"testing"
)

func workflowPath(name string) string {
	return filepath.Join("..", "..", "..", "..", "data", "workflows", "comfyui", name)
}

func mustLoadWorkflow(t *testing.T, filename string, mode Mode) *Workflow {
	t.Helper()
	workflow, err := LoadWorkflow(workflowPath(filename), mode)
	if err != nil {
		t.Fatal(err)
	}
	return workflow
}

func TestBuildMutatesOnlyAllowedWorkflowFields(t *testing.T) {
	for _, test := range []struct {
		mode     Mode
		filename string
	}{
		{mode: TextToImage, filename: "text-image.json"},
		{mode: ImageToImage, filename: "image-image.json"},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			workflow := mustLoadWorkflow(t, test.filename, test.mode)
			expected := cloneNodes(t, workflow.nodes)
			built, seed, err := workflow.Build(WorkflowBuild{Prompt: "positive", NegativePrompt: "negative"})
			if err != nil {
				t.Fatal(err)
			}
			if test.mode == TextToImage {
				expected["4"].Inputs["text"] = "positive"
				expected["5"].Inputs["text"] = "negative"
				expected["8"].Inputs["seed"] = seed
			} else {
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

func TestLoadAndBuildWorkflows(t *testing.T) {
	text := mustLoadWorkflow(t, "text-image.json", TextToImage)
	built, _, err := text.Build(WorkflowBuild{Prompt: "positive", NegativePrompt: "negative"})
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
	if _, _, err := text.Build(WorkflowBuild{Prompt: "positive", NegativePrompt: "negative", Denoise: ptr(0.6)}); err == nil {
		t.Fatal("text-to-image accepted a denoise override")
	}
	if text.nodes["4"].Inputs["text"] == "positive" {
		t.Fatal("template was mutated")
	}

	image := mustLoadWorkflow(t, "image-image.json", ImageToImage)
	built, _, err = image.Build(WorkflowBuild{Prompt: "transform"})
	if err != nil {
		t.Fatal(err)
	}
	if built["4"].Inputs["image"] != InputImageReference || built["9"].Inputs["sampler_name"] != "euler" || built["9"].Inputs["scheduler"] != "sgm_uniform" {
		t.Fatalf("image workflow contract changed: %+v", built)
	}
	if built["12"].Inputs["width"] != float64(1024) || built["12"].Inputs["height"] != float64(1024) {
		t.Fatalf("image resize bounds changed: %+v", built["12"].Inputs)
	}
}

func TestWorkflowValidationRejectsUnsafeBounds(t *testing.T) {
	w := mustLoadWorkflow(t, "text-image.json", TextToImage)
	w.nodes["7"].Inputs["width"] = float64(4096)
	if err := w.validate(); err == nil {
		t.Fatal("unsafe dimensions accepted")
	}
	w = mustLoadWorkflow(t, "image-image.json", ImageToImage)
	w.nodes["12"].Inputs["height"] = float64(32)
	if err := w.validate(); err == nil {
		t.Fatal("undersized image resize accepted")
	}
}

func TestBuildAppliesSamplingOverrides(t *testing.T) {
	seed := uint32(42)
	steps := 12
	cfg := 3.5
	sampler := "dpmpp_2m"
	scheduler := "karras"
	shift := 4.5
	for _, test := range []struct {
		mode      Mode
		filename  string
		samplerID string
		shiftID   string
	}{
		{mode: TextToImage, filename: "text-image.json", samplerID: "8", shiftID: "6"},
		{mode: ImageToImage, filename: "image-image.json", samplerID: "9", shiftID: "8"},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			workflow := mustLoadWorkflow(t, test.filename, test.mode)
			original := cloneNodes(t, workflow.nodes)
			expected := cloneNodes(t, workflow.nodes)
			built, gotSeed, err := workflow.Build(WorkflowBuild{
				Prompt: "positive", NegativePrompt: "negative",
				Seed: &seed, Steps: &steps, CFG: &cfg, SamplerName: &sampler, Scheduler: &scheduler, Shift: &shift,
			})
			if err != nil {
				t.Fatal(err)
			}
			if gotSeed != seed {
				t.Fatalf("seed = %d, want %d", gotSeed, seed)
			}
			samplerInputs := expected[test.samplerID].Inputs
			samplerInputs["seed"] = seed
			samplerInputs["steps"] = steps
			samplerInputs["cfg"] = cfg
			samplerInputs["sampler_name"] = sampler
			samplerInputs["scheduler"] = scheduler
			expected[test.shiftID].Inputs["shift"] = shift
			if test.mode == TextToImage {
				expected["4"].Inputs["text"] = "positive"
				expected["5"].Inputs["text"] = "negative"
			} else {
				expected["6"].Inputs["text"] = "positive"
				expected["7"].Inputs["text"] = "negative"
				expected["4"].Inputs["image"] = InputImageReference
			}
			if !reflect.DeepEqual(built, expected) {
				t.Fatalf("override graph differs outside approved fields\nbuilt=%+v\nwant=%+v", built, expected)
			}
			if !reflect.DeepEqual(workflow.nodes, original) {
				t.Fatal("operator template mutated")
			}
		})
	}
}

func TestBuildRejectsInvalidOverrides(t *testing.T) {
	workflow := mustLoadWorkflow(t, "image-image.json", ImageToImage)
	for _, test := range []struct {
		name  string
		build WorkflowBuild
	}{
		{name: "steps zero", build: WorkflowBuild{Steps: ptr(0)}},
		{name: "steps high", build: WorkflowBuild{Steps: ptr(51)}},
		{name: "cfg low", build: WorkflowBuild{CFG: ptr(-0.1)}},
		{name: "cfg high", build: WorkflowBuild{CFG: ptr(10.1)}},
		{name: "cfg nan", build: WorkflowBuild{CFG: ptr(math.NaN())}},
		{name: "cfg infinite", build: WorkflowBuild{CFG: ptr(math.Inf(1))}},
		{name: "sampler unsupported", build: WorkflowBuild{SamplerName: ptr("not-a-sampler")}},
		{name: "scheduler unsupported", build: WorkflowBuild{Scheduler: ptr("not-a-scheduler")}},
		{name: "shift low", build: WorkflowBuild{Shift: ptr(0.0)}},
		{name: "shift high", build: WorkflowBuild{Shift: ptr(10.1)}},
		{name: "shift nan", build: WorkflowBuild{Shift: ptr(math.NaN())}},
		{name: "denoise zero", build: WorkflowBuild{Denoise: ptr(0.0)}},
		{name: "denoise high", build: WorkflowBuild{Denoise: ptr(0.91)}},
		{name: "denoise nan", build: WorkflowBuild{Denoise: ptr(math.NaN())}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.build.Prompt = "positive"
			if _, _, err := workflow.Build(test.build); err == nil {
				t.Fatal("invalid override accepted")
			}
		})
	}
}

func ptr[T any](value T) *T {
	return &value
}
