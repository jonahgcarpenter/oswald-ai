package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
)

func TestImageToolStreamPayloadOmitsPrivateResults(t *testing.T) {
	for _, name := range []string{toolnames.WebImageSearch, toolnames.WebImageSelect} {
		for _, result := range []string{"", `{"result_id":"private-id","url":"https://private.example","title":"private-title","bytes":"private-bytes"}`, "private-error"} {
			for _, isError := range []bool{false, true} {
				payload := toolStreamPayload(name, map[string]interface{}{
					"query": " cats ", "result_id": "private-id", "url": "https://private.example",
					"title": "private-title", "bytes": []byte("private-bytes"),
				}, result, time.Millisecond, isError)
				if payload.Name != name || payload.DurationMS != 1 || payload.IsError != isError || payload.ResultText != "" {
					t.Fatalf("unexpected status metadata: %+v", payload)
				}
				if name == toolnames.WebImageSearch {
					if len(payload.Arguments) != 1 || payload.Arguments["query"] != "cats" {
						t.Fatalf("expected query-only arguments: %+v", payload.Arguments)
					}
				} else if payload.Arguments != nil {
					t.Fatalf("selection arguments exposed: %+v", payload.Arguments)
				}
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "result_id") {
					t.Fatalf("private data exposed: %s", encoded)
				}
			}
		}
	}
}
