package searxng

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := NewClient(baseURL, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestNewClientValidatesBaseURL(t *testing.T) {
	for _, value := range []string{"", "localhost:8080", "ftp://example.com", "https:///missing-host", "https://user@example.com", "https://example.com?q=1", "https://example.com/#fragment"} {
		if _, err := NewClient(value, nil); err == nil {
			t.Errorf("accepted invalid SearXNG URL %q", value)
		}
	}
	for _, value := range []string{"https://example.com/searx/", "HTTPS://example.com"} {
		if client, err := NewClient(value, nil); err != nil || client == nil {
			t.Fatalf("client = %v, err = %v", client, err)
		}
	}
}

func TestClientBuildsPrefixedSearchRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/search" || r.URL.Query().Get("q") != "go test" || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("language") != "en-US" || r.URL.Query().Get("categories") != "general" || r.URL.Query().Get("pageno") != "1" || r.URL.Query().Has("engines") || r.URL.Query().Has("safesearch") {
			t.Errorf("unexpected request: %s", r.URL.String())
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "oswald-ai/web.search" {
			t.Errorf("unexpected headers: %v", r.Header)
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	if _, err := newTestClient(t, server.URL+"/prefix/").Search(context.Background(), "go test"); err != nil {
		t.Fatal(err)
	}
}

func TestClientValidatesQuery(t *testing.T) {
	client := newTestClient(t, "https://example.com")
	for _, query := range []string{"", "line\nbreak", strings.Repeat("x", 401), strings.Repeat("word ", 51)} {
		if _, err := client.Search(context.Background(), query); err == nil {
			t.Errorf("accepted invalid query %q", query)
		}
	}
	if err := web.ValidateQuery(strings.Repeat("界", 400)); err != nil {
		t.Fatal(err)
	}
}

func TestClientNormalizesAndInspectsOnlyFirstFifty(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"unresponsive_engines":[["slow","timeout"]],"results":[`)
	for i := 0; i < 55; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"title":"%d","url":"https://host%d.example/a?utm_source=x&keep=1#frag"}`, i, i)
	}
	body.WriteString(`]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body.String())) }))
	defer server.Close()
	response, err := newTestClient(t, server.URL).Search(context.Background(), "test")
	if err != nil || !response.Degraded || response.Stats.CandidateCount != 55 || response.Stats.InspectedCount != 50 || len(response.Results) != 50 || response.Results[0].URL != "https://host0.example/a?keep=1" || response.Results[49].Title != "49" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestClientRetriesAndRejectsCrossOriginRedirect(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "private body", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	if _, err := newTestClient(t, server.URL).Search(context.Background(), "private query"); err != nil || attempts.Load() != 2 {
		t.Fatalf("attempts=%d err=%v", attempts.Load(), err)
	}
	destinationCalls := atomic.Int32{}
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer source.Close()
	if _, err := newTestClient(t, source.URL).Search(context.Background(), "test"); err == nil || destinationCalls.Load() != 0 {
		t.Fatalf("cross-origin redirect: calls=%d err=%v", destinationCalls.Load(), err)
	}
}

func TestClientBoundsBodyAndHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("q") {
		case "large":
			_, _ = w.Write([]byte(strings.Repeat("x", web.MaxResponseBytes+1)))
		case "invalid":
			_, _ = w.Write([]byte("not-json"))
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "busy", http.StatusTooManyRequests)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	if _, err := client.Search(context.Background(), "large"); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("large: %v", err)
	}
	if _, err := client.Search(context.Background(), "invalid"); err == nil || !strings.Contains(err.Error(), "parse SearXNG") {
		t.Fatalf("invalid: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := client.Search(ctx, "test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
	if got := retryDelay("9223372036854775807", time.Now()); got != maxRetryDelay {
		t.Fatalf("retry delay: %v", got)
	}
}

func TestClientTelemetryDoesNotLogPrivateData(t *testing.T) {
	const canary = "private_provider_prose"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"` + canary + `","url":"https://example.com/` + canary + `","content":"` + canary + `"}],"unresponsive_engines":[["` + canary + `","timeout"]]}`))
	}))
	defer server.Close()
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	client, err := NewClient(server.URL, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: "req_search", OperationID: "op_parent"})
	if _, err := client.Search(ctx, canary); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), canary) {
		t.Fatalf("private data logged: %s", output.String())
	}
	attempts, completions := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["event"] == "provider.web.search.attempt.complete" {
			attempts++
		}
		if record["event"] == "provider.web.search.complete" {
			completions++
			if record["level"] != "debug" || record["status"] != "degraded" || record["request_id"] != "req_search" || record["operation_id"] == "op_parent" || record["parent_operation_id"] != "op_parent" {
				t.Errorf("completion=%+v", record)
			}
		}
	}
	if attempts != 1 || completions != 1 {
		t.Fatalf("attempts=%d completions=%d", attempts, completions)
	}
}
