package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListModelsReturnsEntries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-bf-vk"); got != "virtual-key" {
			t.Errorf("x-bf-vk = %q", got)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"  model-alpha  ","context_length":128000,"owned_by":"example"},
			{"id":"model-beta","owned_by":"example"},
			{"id":"","context_length":64000},
			{"id":"model-zero","context_length":0},
			{"id":"model-negative","context_length":-5},
			{"id":"model-extra","context_length":32000,"permissions":[],"pricing":{"prompt":"0"}}
		]}`))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "api-key", "virtual-key", nil)
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	want := []ModelEntry{
		{ID: "model-alpha", Window: 128000},
		{ID: "model-beta"},
		{ID: "model-zero"},
		{ID: "model-negative"},
		{ID: "model-extra", Window: 32000},
	}
	if len(models) != len(want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Fatalf("models[%d] = %+v, want %+v", i, models[i], want[i])
		}
	}
}

func TestListModelsAcceptsNestedContextLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"model-nested","top_provider":{"context_length":200000}}]}`))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "", "", nil)
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0] != (ModelEntry{ID: "model-nested", Window: 200000}) {
		t.Fatalf("models = %+v", models)
	}
}

func TestListModelsAcceptsLimitContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"muse-spark-1.3-contributor","limit":{"context":1048576,"output":131072}},
			{"id":"model-top-context","context":64000},
			{"id":"model-limits","limits":{"context":32000}}
		]}`))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "", "", nil)
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []ModelEntry{
		{ID: "muse-spark-1.3-contributor", Window: 1048576},
		{ID: "model-top-context", Window: 64000},
		{ID: "model-limits", Window: 32000},
	}
	if len(models) != len(want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Fatalf("models[%d] = %+v, want %+v", i, models[i], want[i])
		}
	}
}

func TestListModelsHTTPStatusErrors(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"provider detail that must not leak"}`))
			}))
			defer server.Close()

			client := NewGatewayClient(server.URL, "api-key", "virtual-key", nil)
			_, err := client.ListModels(context.Background())
			if err == nil {
				t.Fatal("ListModels succeeded on a non-success status")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("status %d", status)) {
				t.Fatalf("error = %q, want the status", err)
			}
			if strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "api-key") || strings.Contains(err.Error(), "provider detail") {
				t.Fatalf("error leaked endpoint, credential, or body: %q", err)
			}
		})
	}
}

func TestListModelsMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[`))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "api-key", "virtual-key", nil)
	_, err := client.ListModels(context.Background())
	if !errors.Is(err, errModelsListMalformed) {
		t.Fatalf("error = %v, want errModelsListMalformed", err)
	}
}

func TestListModelsOversizeBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), modelsListMaxBytes+1))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "api-key", "virtual-key", nil)
	_, err := client.ListModels(context.Background())
	if !errors.Is(err, errModelsListOversize) {
		t.Fatalf("error = %v, want errModelsListOversize", err)
	}
}

func TestListModelsContextCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "api-key", "virtual-key", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.ListModels(ctx)
		done <- err
	}()

	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestListModelsTransportErrorIsLaundered(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	serverURL := server.URL
	server.Close()

	client := NewGatewayClient(serverURL, "api-key", "virtual-key", nil)
	_, err := client.ListModels(context.Background())
	if !errors.Is(err, errModelsListUnavailable) {
		t.Fatalf("error = %v, want errModelsListUnavailable", err)
	}
	if strings.Contains(err.Error(), serverURL) || strings.Contains(err.Error(), "api-key") {
		t.Fatalf("error leaked endpoint or credential: %q", err)
	}
}

func TestListModelsNilLogger(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"model-alpha"}]}`))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "api-key", "", nil)
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels with nil logger: %v", err)
	}
	if len(models) != 1 || models[0].ID != "model-alpha" {
		t.Fatalf("models = %+v", models)
	}
}

func TestListModelsEntryCap(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"object":"list","data":[`)
	for i := 0; i < modelsListMaxEntries+50; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":"model-%04d","context_length":%d}`, i, 32000+i)
	}
	body.WriteString(`]}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body.String()))
	}))
	defer server.Close()

	client := NewGatewayClient(server.URL, "", "", nil)
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != modelsListMaxEntries {
		t.Fatalf("models = %d entries, want %d", len(models), modelsListMaxEntries)
	}
	if models[0].ID != "model-0000" || models[len(models)-1].ID != fmt.Sprintf("model-%04d", modelsListMaxEntries-1) {
		t.Fatalf("entry bounds = %q..%q", models[0].ID, models[len(models)-1].ID)
	}
}
