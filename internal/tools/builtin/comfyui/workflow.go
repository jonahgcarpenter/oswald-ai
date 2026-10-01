package comfyui

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
)

type node struct {
	Inputs    map[string]interface{} `json:"inputs"`
	ClassType string                 `json:"class_type"`
	Meta      map[string]interface{} `json:"_meta,omitempty"`
}

// Pinned operator-owned model assets. These are validated from the template and
// are never request-controlled.
const (
	sd3UnetName  = "sd3.5_large_turbo-Q5_0.gguf"
	sd3ClipName1 = "clip_l.safetensors"
	sd3ClipName2 = "t5-v1_1-xxl-encoder-Q5_K_M.gguf"
	sd3ClipType  = "sd3"
	sd3VAEName   = "diffusion_pytorch_model.safetensors"
)

// Request-tunable sampling bounds. Dimensions, batch size, and model files stay
// operator-owned because they change VRAM or the selected model.
const (
	minDimension = 64
	maxDimension = 1024
	minSteps     = 1
	maxSteps     = 50
	minCFG       = 0.0
	maxCFG       = 10.0
	minShift     = 0.1
	maxShift     = 10.0
)

// SamplerNames lists the KSampler samplers a request may select. The order is
// stable so the model-visible schema and validation stay in lockstep.
var SamplerNames = []string{
	"euler",
	"euler_ancestral",
	"heun",
	"dpm_2",
	"dpm_2_ancestral",
	"lms",
	"dpmpp_2s_ancestral",
	"dpmpp_2m",
	"dpmpp_2m_sde",
	"dpmpp_sde",
	"dpmpp_3m_sde",
	"ddim",
	"uni_pc",
	"uni_pc_bh2",
}

// SchedulerNames lists the KSampler schedulers a request may select.
var SchedulerNames = []string{
	"normal",
	"karras",
	"exponential",
	"sgm_uniform",
	"simple",
	"ddim_uniform",
	"beta",
	"linear_quadratic",
	"kl_optimal",
}

// Workflow is an immutable validated API workflow template.
type Workflow struct {
	mode  Mode
	nodes map[string]node
}

// WorkflowBuild carries request-controlled overrides. Nil optional fields
// preserve the operator template value.
type WorkflowBuild struct {
	Prompt         string
	NegativePrompt string
	Seed           *uint32
	Steps          *int
	CFG            *float64
	SamplerName    *string
	Scheduler      *string
	Shift          *float64
	Denoise        *float64 // image-to-image strength; rejected for text-to-image
}

// LoadWorkflow reads and validates a supported workflow template.
func LoadWorkflow(path string, mode Mode) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ComfyUI %s workflow: %w", mode, err)
	}
	var nodes map[string]node
	if err := json.Unmarshal(data, &nodes); err != nil {
		return nil, fmt.Errorf("decode ComfyUI %s workflow: %w", mode, err)
	}
	w := &Workflow{mode: mode, nodes: nodes}
	if err := w.validate(); err != nil {
		return nil, fmt.Errorf("validate ComfyUI %s workflow: %w", mode, err)
	}
	return w, nil
}

// Build returns a deep copy with only request-controlled fields changed.
// An omitted seed is randomized; all other nil fields preserve the template.
func (w *Workflow) Build(build WorkflowBuild) (map[string]node, uint32, error) {
	if err := validateOverrides(w.mode, build); err != nil {
		return nil, 0, err
	}
	encoded, err := json.Marshal(w.nodes)
	if err != nil {
		return nil, 0, fmt.Errorf("clone ComfyUI workflow")
	}
	var nodes map[string]node
	if err := json.Unmarshal(encoded, &nodes); err != nil {
		return nil, 0, fmt.Errorf("clone ComfyUI workflow")
	}
	seed := uint32(0)
	if build.Seed != nil {
		seed = *build.Seed
	} else {
		seedBytes := [4]byte{}
		if _, err := rand.Read(seedBytes[:]); err != nil {
			return nil, 0, fmt.Errorf("generate ComfyUI seed: %w", err)
		}
		seed = binary.BigEndian.Uint32(seedBytes[:])
	}
	if w.mode == TextToImage {
		nodes["4"].Inputs["text"] = build.Prompt
		nodes["5"].Inputs["text"] = build.NegativePrompt
		nodes["8"].Inputs["seed"] = seed
		applySamplerOverrides(nodes["8"].Inputs, nodes["6"].Inputs, build)
	} else {
		nodes["6"].Inputs["text"] = build.Prompt
		nodes["7"].Inputs["text"] = build.NegativePrompt
		nodes["9"].Inputs["seed"] = seed
		applySamplerOverrides(nodes["9"].Inputs, nodes["8"].Inputs, build)
		if build.Denoise != nil {
			nodes["9"].Inputs["denoise"] = *build.Denoise
		}
		nodes["4"].Inputs["image"] = InputImageReference
	}
	return nodes, seed, nil
}

func applySamplerOverrides(sampler, sampling map[string]interface{}, build WorkflowBuild) {
	if build.Steps != nil {
		sampler["steps"] = *build.Steps
	}
	if build.CFG != nil {
		sampler["cfg"] = *build.CFG
	}
	if build.SamplerName != nil {
		sampler["sampler_name"] = *build.SamplerName
	}
	if build.Scheduler != nil {
		sampler["scheduler"] = *build.Scheduler
	}
	if build.Shift != nil {
		sampling["shift"] = *build.Shift
	}
}

func validateOverrides(mode Mode, build WorkflowBuild) error {
	if build.Steps != nil && (*build.Steps < minSteps || *build.Steps > maxSteps) {
		return fmt.Errorf("steps must be between %d and %d", minSteps, maxSteps)
	}
	if build.CFG != nil && (math.IsNaN(*build.CFG) || math.IsInf(*build.CFG, 0) || *build.CFG < minCFG || *build.CFG > maxCFG) {
		return fmt.Errorf("cfg must be between %v and %v", minCFG, maxCFG)
	}
	if build.SamplerName != nil && !containsString(SamplerNames, *build.SamplerName) {
		return fmt.Errorf("sampler_name is not supported")
	}
	if build.Scheduler != nil && !containsString(SchedulerNames, *build.Scheduler) {
		return fmt.Errorf("scheduler is not supported")
	}
	if build.Shift != nil && (math.IsNaN(*build.Shift) || math.IsInf(*build.Shift, 0) || *build.Shift < minShift || *build.Shift > maxShift) {
		return fmt.Errorf("shift must be between %v and %v", minShift, maxShift)
	}
	if build.Denoise != nil {
		if mode != ImageToImage {
			return fmt.Errorf("strength is only supported for image-to-image")
		}
		if math.IsNaN(*build.Denoise) || math.IsInf(*build.Denoise, 0) || *build.Denoise < 0.1 || *build.Denoise > 0.9 {
			return fmt.Errorf("strength must be a finite number between 0.1 and 0.9 for image-to-image")
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (w *Workflow) validate() error {
	switch w.mode {
	case TextToImage:
		for id, class := range map[string]string{
			"1": "UnetLoaderGGUF", "2": "DualCLIPLoaderGGUF", "3": "VAELoader",
			"4": "CLIPTextEncode", "5": "CLIPTextEncode", "6": "ModelSamplingSD3",
			"7": "EmptySD3LatentImage", "8": "KSampler", "9": "VAEDecode", "10": "PreviewImage",
		} {
			if err := w.requireNode(id, class); err != nil {
				return err
			}
		}
		if err := w.validateModelAssets(); err != nil {
			return err
		}
		if err := boundedNumber(w.nodes["6"].Inputs, "shift", minShift, maxShift); err != nil {
			return err
		}
		if err := exactNumber(w.nodes["7"].Inputs, "batch_size", 1); err != nil {
			return err
		}
		if err := dimensions(w.nodes["7"].Inputs); err != nil {
			return err
		}
		if err := stringInput(w.nodes["4"].Inputs, "text"); err != nil {
			return err
		}
		if err := stringInput(w.nodes["5"].Inputs, "text"); err != nil {
			return err
		}
		if err := validateSamplerLabels(w.nodes["8"].Inputs); err != nil {
			return err
		}
		if err := validateConnections([]connectionCheck{
			{w.nodes["4"].Inputs, "clip", "2", 0},
			{w.nodes["5"].Inputs, "clip", "2", 0},
			{w.nodes["6"].Inputs, "model", "1", 0},
			{w.nodes["8"].Inputs, "model", "6", 0},
			{w.nodes["8"].Inputs, "positive", "4", 0},
			{w.nodes["8"].Inputs, "negative", "5", 0},
			{w.nodes["8"].Inputs, "latent_image", "7", 0},
			{w.nodes["9"].Inputs, "samples", "8", 0},
			{w.nodes["9"].Inputs, "vae", "3", 0},
			{w.nodes["10"].Inputs, "images", "9", 0},
		}); err != nil {
			return err
		}
		if err := samplerBounds(w.nodes["8"].Inputs, false); err != nil {
			return err
		}
		return exactNumber(w.nodes["8"].Inputs, "denoise", 1)
	case ImageToImage:
		for id, class := range map[string]string{
			"1": "UnetLoaderGGUF", "2": "DualCLIPLoaderGGUF", "3": "VAELoader",
			"4": "LoadImage", "5": "VAEEncode", "6": "CLIPTextEncode", "7": "CLIPTextEncode",
			"8": "ModelSamplingSD3", "9": "KSampler", "10": "VAEDecode", "11": "PreviewImage", "12": "ImageScale",
		} {
			if err := w.requireNode(id, class); err != nil {
				return err
			}
		}
		if err := w.validateModelAssets(); err != nil {
			return err
		}
		if err := boundedNumber(w.nodes["8"].Inputs, "shift", minShift, maxShift); err != nil {
			return err
		}
		if err := dimensions(w.nodes["12"].Inputs); err != nil {
			return err
		}
		if err := stringInput(w.nodes["4"].Inputs, "image"); err != nil {
			return err
		}
		if err := stringInput(w.nodes["6"].Inputs, "text"); err != nil {
			return err
		}
		if err := stringInput(w.nodes["7"].Inputs, "text"); err != nil {
			return err
		}
		if err := validateSamplerLabels(w.nodes["9"].Inputs); err != nil {
			return err
		}
		if err := validateConnections([]connectionCheck{
			{w.nodes["5"].Inputs, "pixels", "12", 0},
			{w.nodes["5"].Inputs, "vae", "3", 0},
			{w.nodes["6"].Inputs, "clip", "2", 0},
			{w.nodes["7"].Inputs, "clip", "2", 0},
			{w.nodes["8"].Inputs, "model", "1", 0},
			{w.nodes["9"].Inputs, "model", "8", 0},
			{w.nodes["9"].Inputs, "positive", "6", 0},
			{w.nodes["9"].Inputs, "negative", "7", 0},
			{w.nodes["9"].Inputs, "latent_image", "5", 0},
			{w.nodes["10"].Inputs, "samples", "9", 0},
			{w.nodes["10"].Inputs, "vae", "3", 0},
			{w.nodes["11"].Inputs, "images", "10", 0},
			{w.nodes["12"].Inputs, "image", "4", 0},
		}); err != nil {
			return err
		}
		return samplerBounds(w.nodes["9"].Inputs, true)
	default:
		return fmt.Errorf("unsupported workflow mode %q", w.mode)
	}
}

func (w *Workflow) validateModelAssets() error {
	if err := exactString(w.nodes["1"].Inputs, "unet_name", sd3UnetName); err != nil {
		return err
	}
	if err := exactString(w.nodes["2"].Inputs, "clip_name1", sd3ClipName1); err != nil {
		return err
	}
	if err := exactString(w.nodes["2"].Inputs, "clip_name2", sd3ClipName2); err != nil {
		return err
	}
	if err := exactString(w.nodes["2"].Inputs, "type", sd3ClipType); err != nil {
		return err
	}
	return exactString(w.nodes["3"].Inputs, "vae_name", sd3VAEName)
}

func (w *Workflow) requireNode(id, class string) error {
	n, ok := w.nodes[id]
	if !ok || n.ClassType != class || n.Inputs == nil {
		return fmt.Errorf("node %s must be %s with inputs", id, class)
	}
	return nil
}

func exactString(inputs map[string]interface{}, key, want string) error {
	if got, ok := inputs[key].(string); !ok || got != want {
		return fmt.Errorf("%s must be %q", key, want)
	}
	return nil
}

func stringInput(inputs map[string]interface{}, key string) error {
	if _, ok := inputs[key].(string); !ok {
		return fmt.Errorf("%s must be a string", key)
	}
	return nil
}

func exactNumber(inputs map[string]interface{}, key string, want float64) error {
	if got, ok := inputs[key].(float64); !ok || got != want {
		return fmt.Errorf("%s must be %v", key, want)
	}
	return nil
}

func boundedNumber(inputs map[string]interface{}, key string, minimum, maximum float64) error {
	got, ok := inputs[key].(float64)
	if !ok || math.IsNaN(got) || math.IsInf(got, 0) || got < minimum || got > maximum {
		return fmt.Errorf("%s must be between %v and %v", key, minimum, maximum)
	}
	return nil
}

func dimensions(inputs map[string]interface{}) error {
	if err := boundedInteger(inputs, "width", minDimension, maxDimension); err != nil {
		return err
	}
	if err := boundedInteger(inputs, "height", minDimension, maxDimension); err != nil {
		return err
	}
	width := int(inputs["width"].(float64))
	height := int(inputs["height"].(float64))
	if width%64 != 0 || height%64 != 0 {
		return fmt.Errorf("dimensions must be multiples of 64")
	}
	if width*height > maxDimension*maxDimension {
		return fmt.Errorf("dimensions exceed the safe generation limit")
	}
	return nil
}

// validateSamplerLabels requires sampler_name and scheduler to be strings. The
// operator template may use any ComfyUI value; request overrides are separately
// restricted to the supported enums.
func validateSamplerLabels(inputs map[string]interface{}) error {
	if err := stringInput(inputs, "sampler_name"); err != nil {
		return err
	}
	return stringInput(inputs, "scheduler")
}

func samplerBounds(inputs map[string]interface{}, imageMode bool) error {
	if err := boundedInteger(inputs, "steps", minSteps, maxSteps); err != nil {
		return err
	}
	if err := boundedNumber(inputs, "cfg", minCFG, maxCFG); err != nil {
		return err
	}
	if imageMode {
		return boundedNumber(inputs, "denoise", 0.1, 0.9)
	}
	return nil
}

type connectionCheck struct {
	inputs map[string]interface{}
	key    string
	nodeID string
	output float64
}

func validateConnections(checks []connectionCheck) error {
	for _, check := range checks {
		if !reflect.DeepEqual(check.inputs[check.key], []interface{}{check.nodeID, check.output}) {
			return fmt.Errorf("%s connection is invalid", check.key)
		}
	}
	return nil
}

func boundedInteger(inputs map[string]interface{}, key string, minimum, maximum float64) error {
	if err := boundedNumber(inputs, key, minimum, maximum); err != nil {
		return err
	}
	value := inputs[key].(float64)
	if value != float64(int64(value)) {
		return fmt.Errorf("%s must be an integer", key)
	}
	return nil
}
