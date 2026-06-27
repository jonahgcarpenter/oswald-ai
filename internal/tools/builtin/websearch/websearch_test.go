package websearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

type fakeSearcher struct {
	results []SearchResult
	err     error
	query   string
}

func (f *fakeSearcher) Search(_ context.Context, query string) ([]SearchResult, error) {
	f.query = query
	return f.results, f.err
}

func TestFormatAndParseResultsRoundTrip(t *testing.T) {
	results := []SearchResult{
		{Title: "First", URL: "https://example.com/1", Content: "one"},
		{Title: "Second", URL: "https://example.com/2", Content: "two\nmore"},
	}

	formatted := FormatResults(results)
	parsed := ParseFormattedResults(formatted)
	if len(parsed) != len(results) {
		t.Fatalf("ParseFormattedResults returned %d results, want %d: %q", len(parsed), len(results), formatted)
	}
	for i := range results {
		if parsed[i] != results[i] {
			t.Fatalf("parsed[%d] = %+v, want %+v", i, parsed[i], results[i])
		}
	}
}

func TestFormatAndParseEmptyResults(t *testing.T) {
	formatted := FormatResults(nil)
	if formatted != "No results found." {
		t.Fatalf("FormatResults(nil) = %q", formatted)
	}
	if parsed := ParseFormattedResults(formatted); parsed != nil {
		t.Fatalf("ParseFormattedResults(no results) = %+v, want nil", parsed)
	}
}

func TestNewHandlerValidatesAndPropagatesSearch(t *testing.T) {
	searcher := &fakeSearcher{results: []SearchResult{{Title: "Title", URL: "https://example.com", Content: "body"}}}
	handler := NewHandler(searcher, config.NewLogger(config.LevelError))

	if _, err := handler(context.Background(), map[string]interface{}{"query": ""}); err == nil {
		t.Fatal("empty query returned nil error")
	}
	result, err := handler(context.Background(), map[string]interface{}{"query": "golang"})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if searcher.query != "golang" {
		t.Fatalf("search query = %q, want golang", searcher.query)
	}
	if !strings.Contains(result, "Title") || !strings.Contains(result, "https://example.com") {
		t.Fatalf("handler result missing formatted search data: %q", result)
	}

	searcher.err = errors.New("boom")
	if _, err := handler(context.Background(), map[string]interface{}{"query": "golang"}); err == nil || !strings.Contains(err.Error(), "search failed") {
		t.Fatalf("handler search error = %v, want wrapped search failed error", err)
	}
}

func TestClientSearchUsesSearXNGEndpointAndCapsResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Fatalf("path = %q, want /search", r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); got != "go test" {
			t.Fatalf("query q = %q, want go test", got)
		}
		if got := r.URL.Query().Get("format"); got != "json" {
			t.Fatalf("format = %q, want json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"1","url":"https://example.com/1","content":"one"},
			{"title":"2","url":"https://example.com/2","content":"two"},
			{"title":"3","url":"https://example.com/3","content":"three"},
			{"title":"4","url":"https://example.com/4","content":"four"},
			{"title":"5","url":"https://example.com/5","content":"five"},
			{"title":"6","url":"https://example.com/6","content":"six"}
		]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, config.NewLogger(config.LevelError))
	results, err := client.Search(context.Background(), "go test")
	if err != nil {
		t.Fatalf("Search returned error: %v", err)
	}
	if len(results) != maxResults {
		t.Fatalf("Search returned %d results, want %d", len(results), maxResults)
	}
	if results[0].Title != "1" || results[4].Title != "5" {
		t.Fatalf("unexpected capped results: %+v", results)
	}

	if _, err := url.Parse(server.URL); err != nil {
		t.Fatalf("test server URL invalid: %v", err)
	}
}

func TestClientSearchReturnsHTTPAndDecodeErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "bad json" {
			_, _ = w.Write([]byte(`not-json`))
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := NewClient(server.URL, config.NewLogger(config.LevelError))
	if _, err := client.Search(context.Background(), "down"); err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("HTTP error = %v, want status 503", err)
	}
	if _, err := client.Search(context.Background(), "bad json"); err == nil || !strings.Contains(err.Error(), "parse SearXNG") {
		t.Fatalf("decode error = %v, want parse SearXNG error", err)
	}
}
