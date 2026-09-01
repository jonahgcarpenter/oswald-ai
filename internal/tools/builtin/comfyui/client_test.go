package comfyui

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
	got, degraded, err := client.Generate(context.Background(), map[string]node{}, "9", nil)
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
	_, degraded, err := client.Generate(context.Background(), map[string]node{}, "9", nil)
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
	if _, _, err := client.Generate(context.Background(), map[string]node{}, "30", input); err != nil {
		t.Fatal(err)
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
	if _, _, err := client.Generate(context.Background(), map[string]node{}, "9", nil); err != nil {
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
		_, _, err := client.Generate(ctx, map[string]node{}, "9", nil)
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
			if _, _, err := client.Generate(context.Background(), map[string]node{}, "9", nil); err == nil || cleanup.Load() != 1 {
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
			if _, _, err := client.Generate(context.Background(), map[string]node{}, "9", nil); err == nil || cleanup.Load() != 1 {
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
	go func() { _, _, err := client.Generate(context.Background(), map[string]node{}, "9", nil); done <- err }()
	<-firstHistory
	go func() { _, _, err := client.Generate(context.Background(), map[string]node{}, "9", nil); done <- err }()
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
