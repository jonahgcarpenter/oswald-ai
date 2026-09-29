package web_search

import (
	"context"
	"errors"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/web"
)

func TestHandlerResultLimitDoesNotFillFromFallback(t *testing.T) {
	for _, limit := range []int{1, MaxWebResults} {
		primary := &countingSearcher{response: SearchResponse{Results: []SearchResult{{Title: "first"}, {Title: "second"}}}}
		fallback := &countingSearcher{}
		result, err := NewHandler(web.NewFallbackSearcher(primary, fallback, nil), config.NewLogger(config.LevelError))(context.Background(), map[string]interface{}{"query": "test", "limit": limit})
		if err != nil {
			t.Fatal(err)
		}
		response, err := DecodeToolResponse(result.Content)
		if err != nil || len(response.Results) != min(limit, 2) || result.IsDegraded || fallback.calls != 0 {
			t.Fatalf("response=%+v err=%v fallback_calls=%d", response, err, fallback.calls)
		}
	}
}

func TestHandlerCapsFallbackResultsAndPreservesDegradation(t *testing.T) {
	for _, primaryErr := range []error{nil, errors.New("private provider failure")} {
		primary := &countingSearcher{err: primaryErr}
		fallback := &countingSearcher{response: SearchResponse{Results: make([]SearchResult, MaxWebResults)}}
		result, err := NewHandler(web.NewFallbackSearcher(primary, fallback, nil), config.NewLogger(config.LevelError))(context.Background(), map[string]interface{}{"query": "test", "limit": 1})
		if err != nil {
			t.Fatal(err)
		}
		response, err := DecodeToolResponse(result.Content)
		if err != nil || len(response.Results) != 1 || result.IsDegraded != (primaryErr != nil) || fallback.calls != 1 {
			t.Fatalf("response=%+v err=%v fallback_calls=%d", response, err, fallback.calls)
		}
	}
}
