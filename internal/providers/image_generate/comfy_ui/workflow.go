package comfy_ui

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
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

var checkpointName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.safetensors$`)

// NewWorkflow builds a fixed graph using a safe, operator-selected checkpoint basename.
func NewWorkflow(mode Mode, checkpoint string) (*Workflow, error) {
	if len(checkpoint) > 128 || !checkpointName.MatchString(checkpoint) || strings.Contains(checkpoint, "..") {
		return nil, fmt.Errorf("ComfyUI checkpoint must be a safe .safetensors basename")
	}
	link := func(id string, output float64) []interface{} { return []interface{}{id, output} }
	n := func(class string, inputs map[string]interface{}) node { return node{ClassType: class, Inputs: inputs} }
	var nodes map[string]node
	switch mode {
	case TextToImage:
		nodes = map[string]node{
			"3": n("KSampler", map[string]interface{}{"seed": float64(0), "steps": float64(20), "cfg": float64(7), "sampler_name": "dpmpp_2m", "scheduler": "karras", "denoise": float64(1), "model": link("4", 0), "positive": link("6", 0), "negative": link("7", 0), "latent_image": link("5", 0)}),
			"4": n("CheckpointLoaderSimple", map[string]interface{}{"ckpt_name": checkpoint}),
			"5": n("EmptyLatentImage", map[string]interface{}{"width": float64(512), "height": float64(512), "batch_size": float64(1)}),
			"6": n("CLIPTextEncode", map[string]interface{}{"text": "", "clip": link("4", 1)}),
			"7": n("CLIPTextEncode", map[string]interface{}{"text": "", "clip": link("4", 1)}),
			"8": n("VAEDecode", map[string]interface{}{"samples": link("3", 0), "vae": link("4", 2)}),
			"9": n("SaveImage", map[string]interface{}{"filename_prefix": "ComfyUI", "images": link("8", 0)}),
		}
	case ImageToImage:
		nodes = map[string]node{
			"23": n("CheckpointLoaderSimple", map[string]interface{}{"ckpt_name": checkpoint}),
			"24": n("CLIPTextEncode", map[string]interface{}{"text": "", "clip": link("23", 1)}),
			"25": n("CLIPTextEncode", map[string]interface{}{"text": "", "clip": link("23", 1)}),
			"26": n("KSampler", map[string]interface{}{"seed": float64(0), "steps": float64(15), "cfg": float64(5), "sampler_name": "dpmpp_2m", "scheduler": "karras", "denoise": 0.45, "model": link("23", 0), "positive": link("24", 0), "negative": link("25", 0), "latent_image": link("28", 0)}),
			"27": n("VAEDecode", map[string]interface{}{"samples": link("26", 0), "vae": link("23", 2)}),
			"28": n("VAEEncode", map[string]interface{}{"pixels": link("32", 0), "vae": link("23", 2)}),
			"29": n("LoadImage", map[string]interface{}{"image": InputImageReference}),
			"30": n("PreviewImage", map[string]interface{}{"images": link("27", 0)}),
			"32": n("ImageScale", map[string]interface{}{"upscale_method": "lanczos", "width": float64(512), "height": float64(512), "crop": "center", "image": link("29", 0)}),
		}
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
		width, height = 768, 432
	case "square":
		width, height = 512, 512
	case "portrait":
		width, height = 432, 768
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
		nodes["5"].Inputs["width"], nodes["5"].Inputs["height"] = width, height
		nodes["6"].Inputs["text"] = prompt
		nodes["7"].Inputs["text"] = negativePrompt
		nodes["3"].Inputs["seed"] = seed
	} else {
		nodes["32"].Inputs["width"], nodes["32"].Inputs["height"] = width, height
		nodes["24"].Inputs["text"] = prompt
		nodes["25"].Inputs["text"] = negativePrompt
		nodes["26"].Inputs["seed"] = seed
		if strength != nil {
			nodes["26"].Inputs["denoise"] = *strength
		}
		nodes["29"].Inputs["image"] = InputImageReference
	}
	if w.mode == ImageToImage {
		value := nodes["26"].Inputs["denoise"].(float64)
		return nodes, seed, &value, nil
	}
	return nodes, seed, nil, nil
}

func (w *Workflow) validate() error {
	switch w.mode {
	case TextToImage:
		if err := w.requireNode("3", "KSampler"); err != nil {
			return err
		}
		if err := w.requireNode("4", "CheckpointLoaderSimple"); err != nil {
			return err
		}
		if err := w.requireNode("5", "EmptyLatentImage"); err != nil {
			return err
		}
		if err := w.requireNode("6", "CLIPTextEncode"); err != nil {
			return err
		}
		if err := w.requireNode("7", "CLIPTextEncode"); err != nil {
			return err
		}
		if err := w.requireNode("8", "VAEDecode"); err != nil {
			return err
		}
		if err := w.requireNode("9", "SaveImage"); err != nil {
			return err
		}
		if err := exactNumber(w.nodes["5"].Inputs, "batch_size", 1); err != nil {
			return err
		}
		if err := dimensions(w.nodes["5"].Inputs); err != nil {
			return err
		}
		if err := stringInput(w.nodes["6"].Inputs, "text"); err != nil {
			return err
		}
		if err := stringInput(w.nodes["7"].Inputs, "text"); err != nil {
			return err
		}
		if err := validateConnections([]connectionCheck{
			{w.nodes["3"].Inputs, "model", "4", 0},
			{w.nodes["3"].Inputs, "positive", "6", 0},
			{w.nodes["3"].Inputs, "negative", "7", 0},
			{w.nodes["3"].Inputs, "latent_image", "5", 0},
			{w.nodes["6"].Inputs, "clip", "4", 1},
			{w.nodes["7"].Inputs, "clip", "4", 1},
			{w.nodes["8"].Inputs, "samples", "3", 0},
			{w.nodes["8"].Inputs, "vae", "4", 2},
			{w.nodes["9"].Inputs, "images", "8", 0},
		}); err != nil {
			return err
		}
		if err := samplerBounds(w.nodes["3"].Inputs, false); err != nil {
			return err
		}
		return exactNumber(w.nodes["3"].Inputs, "denoise", 1)
	case ImageToImage:
		if err := w.requireNode("23", "CheckpointLoaderSimple"); err != nil {
			return err
		}
		if err := w.requireNode("24", "CLIPTextEncode"); err != nil {
			return err
		}
		if err := w.requireNode("25", "CLIPTextEncode"); err != nil {
			return err
		}
		if err := w.requireNode("26", "KSampler"); err != nil {
			return err
		}
		if err := w.requireNode("27", "VAEDecode"); err != nil {
			return err
		}
		if err := w.requireNode("28", "VAEEncode"); err != nil {
			return err
		}
		if err := w.requireNode("29", "LoadImage"); err != nil {
			return err
		}
		if err := w.requireNode("30", "PreviewImage"); err != nil {
			return err
		}
		if err := w.requireNode("32", "ImageScale"); err != nil {
			return err
		}
		if err := exactString(w.nodes["26"].Inputs, "sampler_name", "dpmpp_2m"); err != nil {
			return err
		}
		if err := exactString(w.nodes["26"].Inputs, "scheduler", "karras"); err != nil {
			return err
		}
		if err := dimensions(w.nodes["32"].Inputs); err != nil {
			return err
		}
		if err := stringInput(w.nodes["24"].Inputs, "text"); err != nil {
			return err
		}
		if err := stringInput(w.nodes["25"].Inputs, "text"); err != nil {
			return err
		}
		if err := stringInput(w.nodes["29"].Inputs, "image"); err != nil {
			return err
		}
		if err := validateConnections([]connectionCheck{
			{w.nodes["26"].Inputs, "model", "23", 0},
			{w.nodes["26"].Inputs, "positive", "24", 0},
			{w.nodes["26"].Inputs, "negative", "25", 0},
			{w.nodes["26"].Inputs, "latent_image", "28", 0},
			{w.nodes["24"].Inputs, "clip", "23", 1},
			{w.nodes["25"].Inputs, "clip", "23", 1},
			{w.nodes["27"].Inputs, "samples", "26", 0},
			{w.nodes["27"].Inputs, "vae", "23", 2},
			{w.nodes["28"].Inputs, "pixels", "32", 0},
			{w.nodes["28"].Inputs, "vae", "23", 2},
			{w.nodes["32"].Inputs, "image", "29", 0},
			{w.nodes["30"].Inputs, "images", "27", 0},
		}); err != nil {
			return err
		}
		return samplerBounds(w.nodes["26"].Inputs, true)
	default:
		return fmt.Errorf("unsupported workflow mode %q", w.mode)
	}
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
	if !ok || got < minimum || got > maximum {
		return fmt.Errorf("%s must be between %v and %v", key, minimum, maximum)
	}
	return nil
}

func dimensions(inputs map[string]interface{}) error {
	if err := boundedInteger(inputs, "width", 64, 768); err != nil {
		return err
	}
	if err := boundedInteger(inputs, "height", 64, 768); err != nil {
		return err
	}
	width := int(inputs["width"].(float64))
	height := int(inputs["height"].(float64))
	if width%16 != 0 || height%16 != 0 {
		return fmt.Errorf("dimensions must be multiples of 16")
	}
	if !((width <= 768 && height <= 512) || (width <= 512 && height <= 768)) || width*height > 393216 {
		return fmt.Errorf("dimensions exceed the safe generation limit")
	}
	return nil
}

func samplerBounds(inputs map[string]interface{}, imageMode bool) error {
	if err := boundedInteger(inputs, "steps", 1, 25); err != nil {
		return err
	}
	if err := boundedNumber(inputs, "cfg", 1, 10); err != nil {
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
