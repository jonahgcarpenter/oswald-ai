package comfy_ui

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
)

type node struct {
	Inputs    map[string]interface{} `json:"inputs"`
	ClassType string                 `json:"class_type"`
	Meta      map[string]interface{} `json:"_meta,omitempty"`
}

// Workflow is an immutable, runtime-built ComfyUI API graph.
type Workflow struct {
	mode  Mode
	nodes map[string]node
}

// NewWorkflow builds a fixed SD3.5 Large Turbo graph with deployment-owned models.
func NewWorkflow(mode Mode) (*Workflow, error) {
	link := func(id string, output float64) []interface{} { return []interface{}{id, output} }
	n := func(class string, inputs map[string]interface{}) node { return node{ClassType: class, Inputs: inputs} }
	nodes := map[string]node{
		"1": n("UnetLoaderGGUF", map[string]interface{}{"unet_name": "sd3.5_large_turbo-Q5_0.gguf"}),
		"2": n("DualCLIPLoaderGGUF", map[string]interface{}{"clip_name1": "clip_l.safetensors", "clip_name2": "t5-v1_1-xxl-encoder-Q5_K_M.gguf", "type": "sd3"}),
		"3": n("VAELoader", map[string]interface{}{"vae_name": "diffusion_pytorch_model.safetensors"}),
	}
	switch mode {
	case TextToImage:
		nodes["4"] = n("CLIPTextEncode", map[string]interface{}{"text": "", "clip": link("2", 0)})
		nodes["5"] = n("CLIPTextEncode", map[string]interface{}{"text": "", "clip": link("2", 0)})
		nodes["6"] = n("ModelSamplingSD3", map[string]interface{}{"shift": float64(3), "model": link("1", 0)})
		nodes["7"] = n("EmptySD3LatentImage", map[string]interface{}{"width": float64(1280), "height": float64(720), "batch_size": float64(1)})
		nodes["8"] = n("KSampler", map[string]interface{}{"seed": float64(0), "steps": float64(4), "cfg": float64(1), "sampler_name": "euler", "scheduler": "sgm_uniform", "denoise": float64(1), "model": link("6", 0), "positive": link("4", 0), "negative": link("5", 0), "latent_image": link("7", 0)})
		nodes["9"] = n("VAEDecode", map[string]interface{}{"samples": link("8", 0), "vae": link("3", 0)})
		nodes["10"] = n("SaveImage", map[string]interface{}{"filename_prefix": "Oswald/SD35-Turbo-text", "images": link("9", 0)})
	case ImageToImage:
		nodes["4"] = n("LoadImage", map[string]interface{}{"image": InputImageReference})
		nodes["5"] = n("VAEEncode", map[string]interface{}{"pixels": link("12", 0), "vae": link("3", 0)})
		nodes["6"] = n("CLIPTextEncode", map[string]interface{}{"text": "", "clip": link("2", 0)})
		nodes["7"] = n("CLIPTextEncode", map[string]interface{}{"text": "", "clip": link("2", 0)})
		nodes["8"] = n("ModelSamplingSD3", map[string]interface{}{"shift": float64(3), "model": link("1", 0)})
		nodes["9"] = n("KSampler", map[string]interface{}{"seed": float64(0), "steps": float64(4), "cfg": float64(1), "sampler_name": "euler", "scheduler": "sgm_uniform", "denoise": 0.75, "model": link("8", 0), "positive": link("6", 0), "negative": link("7", 0), "latent_image": link("5", 0)})
		nodes["10"] = n("VAEDecode", map[string]interface{}{"samples": link("9", 0), "vae": link("3", 0)})
		nodes["11"] = n("SaveImage", map[string]interface{}{"filename_prefix": "Oswald/SD35-Turbo-edit", "images": link("10", 0)})
		nodes["12"] = n("ImageScale", map[string]interface{}{"upscale_method": "lanczos", "width": float64(1280), "height": float64(720), "crop": "center", "image": link("4", 0)})
	default:
		return nil, fmt.Errorf("unsupported workflow mode %q", mode)
	}
	w := &Workflow{mode: mode, nodes: nodes}
	if err := w.validate(); err != nil {
		return nil, fmt.Errorf("validate ComfyUI %s workflow: %w", mode, err)
	}
	return w, nil
}

// build returns a deep copy with only request-controlled fields changed.
// A nil strength preserves the default denoise; overrides are image-to-image only.
func (w *Workflow) build(prompt, negativePrompt string, strength *float64, aspect string) (map[string]node, uint32, *float64, error) {
	if strength != nil && (w.mode != ImageToImage || math.IsNaN(*strength) || math.IsInf(*strength, 0) || *strength < 0.1 || *strength > 0.9) {
		return nil, 0, nil, fmt.Errorf("strength must be a finite number between 0.1 and 0.9 for image-to-image")
	}
	width, height := float64(0), float64(0)
	switch aspect {
	case "landscape":
		width, height = 1280, 720
	case "square":
		width, height = 1024, 1024
	case "portrait":
		width, height = 720, 1280
	default:
		return nil, 0, nil, fmt.Errorf("unsupported image aspect ratio")
	}
	encoded, err := json.Marshal(w.nodes)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("clone ComfyUI workflow")
	}
	var nodes map[string]node
	if err := json.Unmarshal(encoded, &nodes); err != nil {
		return nil, 0, nil, fmt.Errorf("clone ComfyUI workflow")
	}
	seedBytes := [4]byte{}
	if _, err := rand.Read(seedBytes[:]); err != nil {
		return nil, 0, nil, fmt.Errorf("generate ComfyUI seed: %w", err)
	}
	seed := binary.BigEndian.Uint32(seedBytes[:])
	if w.mode == TextToImage {
		nodes["7"].Inputs["width"], nodes["7"].Inputs["height"] = width, height
		nodes["4"].Inputs["text"] = prompt
		nodes["5"].Inputs["text"] = negativePrompt
		nodes["8"].Inputs["seed"] = seed
	} else {
		nodes["12"].Inputs["width"], nodes["12"].Inputs["height"] = width, height
		nodes["6"].Inputs["text"] = prompt
		nodes["7"].Inputs["text"] = negativePrompt
		nodes["9"].Inputs["seed"] = seed
		if strength != nil {
			nodes["9"].Inputs["denoise"] = *strength
		}
		nodes["4"].Inputs["image"] = InputImageReference
	}
	if w.mode == ImageToImage {
		value := nodes["9"].Inputs["denoise"].(float64)
		return nodes, seed, &value, nil
	}
	return nodes, seed, nil, nil
}

func (w *Workflow) validate() error {
	classes := map[string]string{"1": "UnetLoaderGGUF", "2": "DualCLIPLoaderGGUF", "3": "VAELoader"}
	positive, negative, sampling, sampler, latent, decoded, output := "4", "5", "6", "8", "7", "9", "10"
	dimension := "7"
	switch w.mode {
	case TextToImage:
		classes[latent] = "EmptySD3LatentImage"
	case ImageToImage:
		positive, negative, sampling, sampler, latent, decoded, output = "6", "7", "8", "9", "5", "10", "11"
		dimension = "12"
		classes["4"], classes[latent], classes[dimension] = "LoadImage", "VAEEncode", "ImageScale"
	default:
		return fmt.Errorf("unsupported workflow mode %q", w.mode)
	}
	classes[positive], classes[negative], classes[sampling] = "CLIPTextEncode", "CLIPTextEncode", "ModelSamplingSD3"
	classes[sampler], classes[decoded], classes[output] = "KSampler", "VAEDecode", "SaveImage"
	for id, class := range classes {
		if err := w.requireNode(id, class); err != nil {
			return err
		}
	}
	for _, check := range []struct{ id, key, value string }{
		{"1", "unet_name", "sd3.5_large_turbo-Q5_0.gguf"},
		{"2", "clip_name1", "clip_l.safetensors"},
		{"2", "clip_name2", "t5-v1_1-xxl-encoder-Q5_K_M.gguf"},
		{"2", "type", "sd3"},
		{"3", "vae_name", "diffusion_pytorch_model.safetensors"},
	} {
		if err := exactString(w.nodes[check.id].Inputs, check.key, check.value); err != nil {
			return err
		}
	}
	if err := dimensions(w.nodes[dimension].Inputs); err != nil {
		return err
	}
	for _, id := range []string{positive, negative} {
		if err := stringInput(w.nodes[id].Inputs, "text"); err != nil {
			return err
		}
	}
	if err := exactNumber(w.nodes[sampling].Inputs, "shift", 3); err != nil {
		return err
	}
	connections := []connectionCheck{
		{w.nodes[sampling].Inputs, "model", "1", 0},
		{w.nodes[positive].Inputs, "clip", "2", 0},
		{w.nodes[negative].Inputs, "clip", "2", 0},
		{w.nodes[sampler].Inputs, "model", sampling, 0},
		{w.nodes[sampler].Inputs, "positive", positive, 0},
		{w.nodes[sampler].Inputs, "negative", negative, 0},
		{w.nodes[sampler].Inputs, "latent_image", latent, 0},
		{w.nodes[decoded].Inputs, "samples", sampler, 0},
		{w.nodes[decoded].Inputs, "vae", "3", 0},
		{w.nodes[output].Inputs, "images", decoded, 0},
	}
	if w.mode == TextToImage {
		if err := exactNumber(w.nodes[latent].Inputs, "batch_size", 1); err != nil {
			return err
		}
		if err := exactNumber(w.nodes[sampler].Inputs, "denoise", 1); err != nil {
			return err
		}
	} else {
		if err := stringInput(w.nodes["4"].Inputs, "image"); err != nil {
			return err
		}
		if err := exactString(w.nodes[dimension].Inputs, "upscale_method", "lanczos"); err != nil {
			return err
		}
		if err := exactString(w.nodes[dimension].Inputs, "crop", "center"); err != nil {
			return err
		}
		connections = append(connections,
			connectionCheck{w.nodes[latent].Inputs, "pixels", dimension, 0},
			connectionCheck{w.nodes[latent].Inputs, "vae", "3", 0},
			connectionCheck{w.nodes[dimension].Inputs, "image", "4", 0},
		)
	}
	if err := validateConnections(connections); err != nil {
		return err
	}
	return samplerBounds(w.nodes[sampler].Inputs, w.mode == ImageToImage)
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
	if err := boundedInteger(inputs, "width", 64, 1280); err != nil {
		return err
	}
	if err := boundedInteger(inputs, "height", 64, 1280); err != nil {
		return err
	}
	width := int(inputs["width"].(float64))
	height := int(inputs["height"].(float64))
	if width%16 != 0 || height%16 != 0 {
		return fmt.Errorf("dimensions must be multiples of 16")
	}
	if width*height > 1024*1024 {
		return fmt.Errorf("dimensions exceed the generation pixel limit")
	}
	return nil
}

func samplerBounds(inputs map[string]interface{}, imageMode bool) error {
	if err := exactNumber(inputs, "steps", 4); err != nil {
		return err
	}
	if err := exactNumber(inputs, "cfg", 1); err != nil {
		return err
	}
	if err := exactString(inputs, "sampler_name", "euler"); err != nil {
		return err
	}
	if err := exactString(inputs, "scheduler", "sgm_uniform"); err != nil {
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
