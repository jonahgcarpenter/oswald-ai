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
			_, _ = w.Write([]byte(`{"job-1":{"outputs":{"9":{"images":[{"filename":"result.png","subfolder":"","type":"output"}]}},"status":{"completed":true,"status_str":"success"}}}`))
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
	result, err := generateTest(client, context.Background(), "9", nil)
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
			_, _ = w.Write([]byte(`{"job":{"outputs":{"9":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
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
	result, err := generateTest(client, context.Background(), "9", nil)
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
			_, _ = w.Write([]byte(`{"job":{"outputs":{"30":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
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
	if _, err := generateTest(client, context.Background(), "30", input); err != nil {
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
			_, _ = w.Write([]byte(`{"job":{"outputs":{"30":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
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
	workflow, err := NewWorkflow(ImageToImage, "dreamshaper_8.safetensors")
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
	if submitted["26"].Inputs["denoise"] != strength || submitted["26"].Inputs["seed"] != float64(result.Seed) || submitted["24"].Inputs["text"] != "positive" || submitted["25"].Inputs["text"] != "negative" || submitted["29"].Inputs["image"] != InputImageReference {
		t.Fatalf("submitted workflow does not match generation metadata")
	}
	if workflow.nodes["26"].Inputs["denoise"] == strength {
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
		{TextToImage, "9", false,
			map[string]string{"3": "KSampler", "4": "CheckpointLoaderSimple", "5": "EmptyLatentImage", "6": "CLIPTextEncode", "7": "CLIPTextEncode", "8": "VAEDecode", "9": "SaveImage"},
			map[string]map[string][]interface{}{"3": {"model": {"4", float64(0)}, "positive": {"6", float64(0)}, "negative": {"7", float64(0)}, "latent_image": {"5", float64(0)}}, "6": {"clip": {"4", float64(1)}}, "7": {"clip": {"4", float64(1)}}, "8": {"samples": {"3", float64(0)}, "vae": {"4", float64(2)}}, "9": {"images": {"8", float64(0)}}}},
		{ImageToImage, "30", true,
			map[string]string{"23": "CheckpointLoaderSimple", "24": "CLIPTextEncode", "25": "CLIPTextEncode", "26": "KSampler", "27": "VAEDecode", "28": "VAEEncode", "29": "LoadImage", "30": "PreviewImage", "32": "ImageScale"},
			map[string]map[string][]interface{}{"24": {"clip": {"23", float64(1)}}, "25": {"clip": {"23", float64(1)}}, "26": {"model": {"23", float64(0)}, "positive": {"24", float64(0)}, "negative": {"25", float64(0)}, "latent_image": {"28", float64(0)}}, "27": {"samples": {"26", float64(0)}, "vae": {"23", float64(2)}}, "28": {"pixels": {"32", float64(0)}, "vae": {"23", float64(2)}}, "30": {"images": {"27", float64(0)}}, "32": {"image": {"29", float64(0)}}}},
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
			workflow, err := NewWorkflow(tc.mode, "custom.safetensors")
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
			samID, dimensionsID, checkpointID, positiveID, negativeID := "3", "5", "4", "6", "7"
			if tc.png {
				samID, dimensionsID, checkpointID, positiveID, negativeID = "26", "32", "23", "24", "25"
			}
			for id, fields := range map[string]map[string]interface{}{
				samID:        {"seed": float64(result.Seed), "sampler_name": "dpmpp_2m", "scheduler": "karras"},
				dimensionsID: {"width": float64(432), "height": float64(768)},
				checkpointID: {"ckpt_name": "custom.safetensors"},
				positiveID:   {"text": "prompt"}, negativeID: {"text": "negative"},
			} {
				for key, want := range fields {
					if got := submitted[id].Inputs[key]; got != want {
						t.Errorf("node %s %s = %v, want %v", id, key, got, want)
					}
				}
			}
			if tc.png {
				if submitted["29"].Inputs["image"] != InputImageReference || submitted["32"].Inputs["upscale_method"] != "lanczos" || submitted["32"].Inputs["crop"] != "center" || submitted["26"].Inputs["denoise"] != 0.45 || submitted["26"].Inputs["steps"] != float64(15) || submitted["26"].Inputs["cfg"] != float64(5) {
					t.Fatal("image graph parameters changed")
				}
			} else if submitted["3"].Inputs["denoise"] != float64(1) || submitted["3"].Inputs["steps"] != float64(20) || submitted["3"].Inputs["cfg"] != float64(7) || submitted["5"].Inputs["batch_size"] != float64(1) || submitted["9"].Inputs["filename_prefix"] != "ComfyUI" {
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
	text, err := NewWorkflow(TextToImage, "dreamshaper_8.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	imageWorkflow, err := NewWorkflow(ImageToImage, "dreamshaper_8.safetensors")
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
			_, _ = w.Write([]byte(`{"job":{"outputs":{"9":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
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
	if _, err := generateTest(client, context.Background(), "9", nil); err != nil {
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
		_, err := generateTest(client, ctx, "9", nil)
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
		{name: "path traversal", history: `{"job":{"outputs":{"9":{"images":[{"filename":"../secret.png","type":"output"}]}}}}`},
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
			if _, err := generateTest(client, context.Background(), "9", nil); err == nil || cleanup.Load() != 1 {
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
					_, _ = w.Write([]byte(`{"job":{"outputs":{"9":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
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
			if _, err := generateTest(client, context.Background(), "9", nil); err == nil || cleanup.Load() != 1 {
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
			_, _ = w.Write([]byte(`{"job":{"outputs":{"9":{"images":[{"filename":"x.png","type":"output"}]}}}}`))
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
	go func() { _, err := generateTest(client, context.Background(), "9", nil); done <- err }()
	<-firstHistory
	go func() { _, err := generateTest(client, context.Background(), "9", nil); done <- err }()
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
	if outputNode == "30" {
		mode = ImageToImage
	}
	workflow, err := NewWorkflow(mode, "dreamshaper_8.safetensors")
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
