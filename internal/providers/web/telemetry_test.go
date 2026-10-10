package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestFallbackCompletionReportsFinalDegradation(t *testing.T) {
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	primary := &countingSearcher{err: errors.New("private fallback error prose")}
	fallback := &countingSearcher{response: SearchResponse{Results: []SearchResult{{Title: "private title prose"}}}}
	response, err := NewFallbackSearcher(primary, fallback, log).Search(context.Background(), "private query prose")
	if err != nil || !response.Degraded {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r["event"] == "provider.web.search.fallback.complete" {
			found = true
			d := logDetails(t, r)
			if r["level"] != "debug" || d["status"] != "degraded" || d["result_count"] != float64(1) {
				t.Errorf("record=%+v", r)
			}
		}
	}
	if !found || strings.Contains(output.String(), "private") {
		t.Fatalf("logs=%s", output.String())
	}
}

func logDetails(t *testing.T, record map[string]any) map[string]any {
	t.Helper()
	details, ok := record["details"].(map[string]any)
	if !ok {
		t.Fatalf("missing details object: %#v", record)
	}
	return details
}
