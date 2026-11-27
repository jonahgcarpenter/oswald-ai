package comfy_ui

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientPollsPendingHistoryDownloadsAndCleansUp(t *testing.T) {
	imageData := testPNG(t)
	var historyCalls atomic.Int32
	var cleanupCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prompt":
			var payload struct {
				ClientID string `json:"client_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.ClientID != "oswald-ai" {
				t.Errorf("prompt payload=%+v err=%v", payload, err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"job-1"}`))
		case "/history/job-1":
			w.Header().Set("Content-Type", "application/json")
			if historyCalls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_, _ = w.Write([]byte(`{"job-1":{"outputs":{"10":{"images":[{"filename":"result.png","subfolder":"","type":"output"}]}},"status":{"completed":true,"status_str":"success"}}}`))
		case "/view":
			if r.URL.Query().Get("filename") != "result.png" {
				t.Errorf("unexpected view query: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(imageData)
		case "/free":
			cleanupCalls.Add(1)
			var payload map[string]bool
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || !payload["free_memory"] || !payload["unload_models"] {
				t.Errorf("cleanup payload = %v, err=%v", payload, err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.pollInterval = time.Millisecond
	result, err := generateTest(client, context.Background(), "10", nil)
	got, degraded := result.Image, result.CleanupFailed
	if err != nil {
		t.Fatal(err)
	}
	if degraded || got.MIMEType != "image/png" || got.Size != image.Pt(2, 3) || cleanupCalls.Load() != 1 {
		t.Fatalf("result=%+v degraded=%t cleanup=%d", got, degraded, cleanupCalls.Load())
	}
}

func TestClientReturnsImageDegradedAfterCleanupRetriesFail(t *testing.T) {
	imageData := testPNG(t)
	var cleanups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prompt":
			_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
		case "/history/job":
			_, _ = w.Write([]byte(`{"job":{"outputs":{"10":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
		case "/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(imageData)
		case "/free":
			cleanups.Add(1)
			http.Error(w, "private provider body", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := generateTest(client, context.Background(), "10", nil)
	degraded := result.CleanupFailed
	if err != nil || !degraded || cleanups.Load() != 2 {
		t.Fatalf("err=%v degraded=%t cleanups=%d", err, degraded, cleanups.Load())
	}
}

func TestClientUploadsFixedPNG(t *testing.T) {
	input := testPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/image":
			if err := r.ParseMultipartForm(int64(len(input) + 4096)); err != nil {
				t.Error(err)
			}
			file, header, err := r.FormFile("image")
			if err != nil {
				t.Error(err)
			} else {
				defer file.Close()
				if header.Filename != InputFilename {
					t.Errorf("filename=%q", header.Filename)
				}
			}
			if r.FormValue("type") != "input" || r.FormValue("subfolder") != InputSubfolder || r.FormValue("overwrite") != "true" {
				t.Errorf("unexpected upload fields: type=%q subfolder=%q overwrite=%q", r.FormValue("type"), r.FormValue("subfolder"), r.FormValue("overwrite"))
			}
			_, _ = w.Write([]byte(`{"name":"` + InputFilename + `","subfolder":"` + InputSubfolder + `","type":"input"}`))
		case "/prompt":
			_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
		case "/history/job":
			_, _ = w.Write([]byte(`{"job":{"outputs":{"11":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
		case "/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(input)
		case "/free":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	if _, err := generateTest(client, context.Background(), "11", input); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateBuildsWorkflowAndReportsEffectiveParameters(t *testing.T) {
	input := testPNG(t)
	var submitted map[string]node
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/image":
			_, _ = w.Write([]byte(`{"name":"` + InputFilename + `","subfolder":"` + InputSubfolder + `","type":"input"}`))
		case "/prompt":
			var payload struct {
				Prompt map[string]node `json:"prompt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			submitted = payload.Prompt
			_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
		case "/history/job":
			_, _ = w.Write([]byte(`{"job":{"outputs":{"11":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
		case "/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(input)
		case "/free":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := NewWorkflow(ImageToImage)
	if err != nil {
		t.Fatal(err)
	}
	strength := 0.6
	result, err := client.Generate(context.Background(), nil, workflow, "positive", "negative", &strength, input, "landscape")
	if err != nil {
		t.Fatal(err)
	}
	if result.CleanupFailed || result.EffectiveStrength == nil || *result.EffectiveStrength != strength || result.Image.MIMEType != "image/png" {
		t.Fatalf("generation metadata = %+v", result)
	}
	if submitted["9"].Inputs["denoise"] != strength || submitted["9"].Inputs["seed"] != float64(result.Seed) || submitted["6"].Inputs["text"] != "positive" || submitted["7"].Inputs["text"] != "negative" || submitted["4"].Inputs["image"] != InputImageReference {
		t.Fatalf("submitted workflow does not match generation metadata")
	}
	if workflow.nodes["9"].Inputs["denoise"] == strength {
		t.Fatal("template was mutated")
	}
}

func TestGenerateSubmitsCompleteRuntimeGraphs(t *testing.T) {
	for _, tc := range []struct {
		mode    Mode
		output  string
		png     bool
		classes map[string]string
		links   map[string]map[string][]interface{}
	}{
		{TextToImage, "10", false,
			map[string]string{"1": "UnetLoaderGGUF", "2": "DualCLIPLoaderGGUF", "3": "VAELoader", "4": "CLIPTextEncode", "5": "CLIPTextEncode", "6": "ModelSamplingSD3", "7": "EmptySD3LatentImage", "8": "KSampler", "9": "VAEDecode", "10": "SaveImage"},
			map[string]map[string][]interface{}{"4": {"clip": {"2", float64(0)}}, "5": {"clip": {"2", float64(0)}}, "6": {"model": {"1", float64(0)}}, "8": {"model": {"6", float64(0)}, "positive": {"4", float64(0)}, "negative": {"5", float64(0)}, "latent_image": {"7", float64(0)}}, "9": {"samples": {"8", float64(0)}, "vae": {"3", float64(0)}}, "10": {"images": {"9", float64(0)}}}},
		{ImageToImage, "11", true,
			map[string]string{"1": "UnetLoaderGGUF", "2": "DualCLIPLoaderGGUF", "3": "VAELoader", "4": "LoadImage", "5": "VAEEncode", "6": "CLIPTextEncode", "7": "CLIPTextEncode", "8": "ModelSamplingSD3", "9": "KSampler", "10": "VAEDecode", "11": "SaveImage", "12": "ImageScale"},
			map[string]map[string][]interface{}{"5": {"pixels": {"12", float64(0)}, "vae": {"3", float64(0)}}, "6": {"clip": {"2", float64(0)}}, "7": {"clip": {"2", float64(0)}}, "8": {"model": {"1", float64(0)}}, "9": {"model": {"8", float64(0)}, "positive": {"6", float64(0)}, "negative": {"7", float64(0)}, "latent_image": {"5", float64(0)}}, "10": {"samples": {"9", float64(0)}, "vae": {"3", float64(0)}}, "11": {"images": {"10", float64(0)}}, "12": {"image": {"4", float64(0)}}}},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			var submitted map[string]node
			imageData := testPNG(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/upload/image":
					_, _ = w.Write([]byte(`{"name":"` + InputFilename + `","subfolder":"` + InputSubfolder + `","type":"input"}`))
				case "/prompt":
					var payload struct {
						Prompt map[string]node `json:"prompt"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					submitted = payload.Prompt
					_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
				case "/history/job":
					_, _ = w.Write([]byte(`{"job":{"outputs":{"` + tc.output + `":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
				case "/view":
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write(imageData)
				case "/free":
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
				}
			}))
			defer server.Close()
			workflow, err := NewWorkflow(tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			client, err := NewClient(server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var input []byte
			if tc.png {
				input = imageData
			}
			result, err := client.Generate(context.Background(), nil, workflow, "prompt", "negative", nil, input, "portrait")
			if err != nil {
				t.Fatal(err)
			}
			if len(submitted) != len(tc.classes) {
				t.Fatalf("submitted %d nodes, want %d", len(submitted), len(tc.classes))
			}
			for id, class := range tc.classes {
				if submitted[id].ClassType != class {
					t.Errorf("node %s class = %q, want %q", id, submitted[id].ClassType, class)
				}
				for key, link := range tc.links[id] {
					if !reflect.DeepEqual(submitted[id].Inputs[key], link) {
						t.Errorf("node %s %s = %v, want %v", id, key, submitted[id].Inputs[key], link)
					}
				}
			}
			samID, dimensionsID, samplingID, positiveID, negativeID := "8", "7", "6", "4", "5"
			if tc.png {
				samID, dimensionsID, samplingID, positiveID, negativeID = "9", "12", "8", "6", "7"
			}
			for id, fields := range map[string]map[string]interface{}{
				samID:        {"seed": float64(result.Seed), "sampler_name": "euler", "scheduler": "sgm_uniform", "steps": float64(4), "cfg": float64(1)},
				dimensionsID: {"width": float64(720), "height": float64(1280)},
				samplingID:   {"shift": float64(3)},
				"1":          {"unet_name": "sd3.5_large_turbo-Q5_0.gguf"},
				"2":          {"clip_name1": "clip_l.safetensors", "clip_name2": "t5-v1_1-xxl-encoder-Q5_K_M.gguf", "type": "sd3"},
				"3":          {"vae_name": "diffusion_pytorch_model.safetensors"},
				positiveID:   {"text": "prompt"}, negativeID: {"text": "negative"},
			} {
				for key, want := range fields {
					if got := submitted[id].Inputs[key]; got != want {
						t.Errorf("node %s %s = %v, want %v", id, key, got, want)
					}
				}
			}
			if tc.png {
				if submitted["4"].Inputs["image"] != InputImageReference || submitted["12"].Inputs["upscale_method"] != "lanczos" || submitted["12"].Inputs["crop"] != "center" || submitted["9"].Inputs["denoise"] != 0.75 || result.EffectiveStrength == nil || *result.EffectiveStrength != 0.75 || submitted["11"].Inputs["filename_prefix"] != "Oswald/SD35-Turbo-edit" {
					t.Fatal("image graph parameters changed")
				}
			} else if submitted["8"].Inputs["denoise"] != float64(1) || result.EffectiveStrength != nil || submitted["7"].Inputs["batch_size"] != float64(1) || submitted["10"].Inputs["filename_prefix"] != "Oswald/SD35-Turbo-text" {
				t.Fatal("text graph parameters changed")
			}
		})
	}
}

func TestGenerateRejectsInvalidInputsBeforeProviderWork(t *testing.T) {
	client, err := NewClient("http://localhost:1234", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	text, err := NewWorkflow(TextToImage)
	if err != nil {
		t.Fatal(err)
	}
	imageWorkflow, err := NewWorkflow(ImageToImage)
	if err != nil {
		t.Fatal(err)
	}
	strength := 0.95
	for _, test := range []struct {
		workflow *Workflow
		strength *float64
		png      []byte
	}{
		{nil, nil, nil}, {text, nil, []byte("png")}, {imageWorkflow, nil, nil}, {imageWorkflow, &strength, []byte("png")},
	} {
		if _, err := client.Generate(context.Background(), nil, test.workflow, "prompt", "", test.strength, test.png, "landscape"); err == nil {
			t.Fatalf("accepted invalid request: workflow=%v strength=%v png=%t", test.workflow, test.strength, test.png != nil)
		}
	}
}

func TestClientCleanupRunsAfterViewBeforeReturn(t *testing.T) {
	var mu sync.Mutex
	var events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prompt":
			_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
		case "/history/job":
			_, _ = w.Write([]byte(`{"job":{"outputs":{"10":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
		case "/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(testPNG(t))
			mu.Lock()
			events = append(events, "view")
			mu.Unlock()
		case "/free":
			mu.Lock()
			events = append(events, "free")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	if _, err := generateTest(client, context.Background(), "10", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	events = append(events, "return")
	mu.Unlock()
	if !reflect.DeepEqual(events, []string{"view", "free", "return"}) {
		t.Fatalf("events=%v", events)
	}
}

func TestClientCancellationStillUsesDetachedCleanupContext(t *testing.T) {
	historyStarted := make(chan struct{})
	cleanupCalled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prompt":
			_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
		case "/history/job":
			close(historyStarted)
			<-r.Context().Done()
		case "/free":
			if r.Context().Err() != nil {
				t.Errorf("cleanup inherited caller cancellation: %v", r.Context().Err())
			}
			cleanupCalled <- struct{}{}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := generateTest(client, ctx, "10", nil)
		done <- err
	}()
	<-historyStarted
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled generation succeeded")
	}
	select {
	case <-cleanupCalled:
	default:
		t.Fatal("cleanup was not called after cancellation")
	}
}

func TestClientCleansUpWorkflowFailureAndUnsafeOutput(t *testing.T) {
	for _, test := range []struct {
		name    string
		history string
	}{
		{name: "terminal failure", history: `{"job":{"status":{"completed":true,"status_str":"error"}}}`},
		{name: "path traversal", history: `{"job":{"outputs":{"10":{"images":[{"filename":"../secret.png","type":"output"}]}}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cleanup atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/prompt":
					_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
				case "/history/job":
					_, _ = w.Write([]byte(test.history))
				case "/free":
					cleanup.Add(1)
					w.WriteHeader(http.StatusOK)
				}
			}))
			defer server.Close()
			client, _ := NewClient(server.URL, time.Second)
			if _, err := generateTest(client, context.Background(), "10", nil); err == nil || cleanup.Load() != 1 {
				t.Fatalf("err=%v cleanup=%d", err, cleanup.Load())
			}
		})
	}
}

func TestClientRejectsInvalidAndOversizedOutputImages(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
		mime string
	}{
		{name: "invalid", body: []byte("not an image"), mime: "image/png"},
		{name: "oversized", body: bytes.Repeat([]byte{'x'}, maxOutputBytesForTest()), mime: "image/png"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cleanup atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/prompt":
					_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
				case "/history/job":
					_, _ = w.Write([]byte(`{"job":{"outputs":{"10":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
				case "/view":
					w.Header().Set("Content-Type", test.mime)
					_, _ = w.Write(test.body)
				case "/free":
					cleanup.Add(1)
					w.WriteHeader(http.StatusOK)
				}
			}))
			defer server.Close()
			client, _ := NewClient(server.URL, time.Second)
			if _, err := generateTest(client, context.Background(), "10", nil); err == nil || cleanup.Load() != 1 {
				t.Fatalf("err=%v cleanup=%d", err, cleanup.Load())
			}
		})
	}
}

func TestClientSerializesGenerationsThroughCleanup(t *testing.T) {
	release := make(chan struct{})
	firstHistory := make(chan struct{})
	var promptCalls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prompt":
			call := promptCalls.Add(1)
			current := active.Add(1)
			if current > maxActive.Load() {
				maxActive.Store(current)
			}
			_, _ = w.Write([]byte(`{"prompt_id":"job"}`))
			if call == 1 {
				return
			}
		case "/history/job":
			if promptCalls.Load() == 1 {
				select {
				case <-firstHistory:
				default:
					close(firstHistory)
				}
				<-release
			}
			_, _ = w.Write([]byte(`{"job":{"outputs":{"10":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
		case "/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(testPNG(t))
		case "/free":
			active.Add(-1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, 2*time.Second)
	done := make(chan error, 2)
	go func() { _, err := generateTest(client, context.Background(), "10", nil); done <- err }()
	<-firstHistory
	go func() { _, err := generateTest(client, context.Background(), "10", nil); done <- err }()
	time.Sleep(25 * time.Millisecond)
	if promptCalls.Load() != 1 {
		t.Fatalf("second generation reached ComfyUI concurrently: calls=%d", promptCalls.Load())
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if maxActive.Load() != 1 {
		t.Fatalf("max active generations=%d", maxActive.Load())
	}
}

func maxOutputBytesForTest() int {
	return 8<<20 + 1
}

func generateTest(client *Client, ctx context.Context, outputNode string, png []byte) (Generation, error) {
	mode := TextToImage
	if outputNode == "11" {
		mode = ImageToImage
	}
	workflow, err := NewWorkflow(mode)
	if err != nil {
		return Generation{}, err
	}
	return client.Generate(ctx, nil, workflow, "positive", "negative", nil, png, "landscape")
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.White)
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
