package websearch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func TestImageSearchResultCountsAndCatalogCapacity(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, tc := range []struct {
			name                      string
			results                   interface{}
			initial, want, downloads  int
			duplicate, reuse, limited bool
			allDuplicate              bool
		}{
			{name: "default", want: 2, downloads: 2},
			{name: "minimum", results: float64(1), want: 1, downloads: 1},
			{name: "maximum", results: float64(4), want: 4, downloads: 4},
			{name: "duplicate_bytes", results: 4, want: 4, downloads: 5, duplicate: true},
			{name: "eight_candidate_scan_bound", results: 4, want: 1, downloads: 8, allDuplicate: true},
			{name: "partial_capacity", results: 4, initial: 3, want: 1, downloads: 4, limited: true},
			{name: "full_omits_new", results: 4, initial: 4, downloads: 4, limited: true},
			{name: "full_reuses", results: 4, initial: 4, want: 4, downloads: 4, reuse: true},
		} {
			t.Run(level.String()+"/"+tc.name, func(t *testing.T) {
				const canary = "private_capacity_canary"
				calls, downloads := 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					results := make([]map[string]any, 10)
					for i := range results {
						results[i] = map[string]any{"title": canary, "url": "https://example.com/" + canary, "thumbnail": map[string]string{"src": fmt.Sprintf("https://example.com/%s/%d", canary, i)}}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "images", "results": results})
				}))
				defer server.Close()
				var logs bytes.Buffer
				log := config.NewLogger(level)
				log.SetOutput(&logs)
				state := requestctx.NewImageSearchState()
				var initial []requestctx.ImageSearchReference
				for i := 1; i <= tc.initial; i++ {
					initial = append(initial, requestctx.ImageSearchReference{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s-old-%d", canary, i)))})
				}
				admitted, err := state.AddReferences(initial)
				if err != nil {
					t.Fatal(err)
				}
				if len(admitted) > 0 {
					state.MarkInspected(admitted[0], admitted[0])
					state.MarkSelected(admitted[0].ID)
				}
				handler := newImageSearchHandler(canary, server.URL, server.Client(), func(context.Context, string) (llm.InputImage, error) {
					downloads++
					i := downloads
					if tc.duplicate && downloads > 1 {
						i--
					}
					if tc.allDuplicate {
						i = 1
					}
					kind := "new"
					if tc.reuse {
						kind = "old"
					}
					return llm.InputImage{MimeType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s-%s-%d", canary, kind, i)))}, nil
				}, log)
				args := map[string]interface{}{"query": canary}
				if tc.results != nil {
					args["results"] = tc.results
				}
				ctx := requestctx.WithImageSearchState(context.Background(), state)
				result, err := handler(ctx, args)
				if err != nil || calls != 1 || downloads != tc.downloads || result.IsDegraded || len(result.Attachments) != 0 {
					t.Fatalf("err=%v calls=%d downloads=%d result=%+v", err, calls, downloads, result)
				}
				var envelope struct {
					Results   []imageResultMetadata `json:"results"`
					Remaining int                   `json:"remaining_slots"`
					Limited   bool                  `json:"catalog_limited"`
					Notice    string                `json:"notice"`
				}
				if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
					t.Fatal(err)
				}
				active := state.ActiveReferences()
				if len(envelope.Results) != tc.want || envelope.Limited != tc.limited || envelope.Remaining != 4-len(active) || len(active) > 4 {
					t.Fatalf("envelope=%+v active=%d", envelope, len(active))
				}
				if (envelope.Remaining == 0) != strings.Contains(envelope.Notice, "catalog is full") {
					t.Fatal("missing or premature capacity notice")
				}
				if tc.limited && result.ReasonCode != "image_catalog_limit" {
					t.Fatal("capacity reason lost")
				}
				if tc.want == 0 && result.Outcome != governance.OutcomeUnproductive {
					t.Fatal("empty admission marked productive")
				}
				if tc.initial > 0 && (len(state.SelectedReferences()) != 1 || state.SelectedReferences()[0].ID != admitted[0].ID) {
					t.Fatal("lost first selection")
				}
				var record map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
					t.Fatal("expected one measurement", err)
				}
				requested, _ := ResultLimit(args, 2, 4)
				if record["requested_result_count"] != float64(requested) || record["is_catalog_limited"] != tc.limited || record["image_count"] != float64(tc.want) || record["candidate_count"] != float64(8) || record["attempted_download_count"] != float64(tc.downloads) || record["failed_count"] != float64(0) || record["status"] != "ok" || record["level"] != "info" {
					t.Fatalf("metrics=%v", record)
				}
				if strings.Contains(logs.String(), canary) || strings.Contains(logs.String(), "https://") || strings.Contains(logs.String(), "search-1") {
					t.Fatal("private telemetry")
				}
				for _, ref := range active {
					if strings.Contains(logs.String(), ref.Data) {
						t.Fatal("preview bytes leaked")
					}
				}
			})
		}
	}
}

func TestImageSearchRejectsInvalidResultsBeforeSubmission(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for i, invalid := range []interface{}{nil, 0, -1, 5, 1.5, "private_results_canary", true, json.Number("private_number_canary"), math.NaN(), math.Inf(1), []int{1}} {
			t.Run(fmt.Sprintf("%s/%d", level.String(), i), func(t *testing.T) {
				var logs bytes.Buffer
				log := config.NewLogger(level)
				log.SetOutput(&logs)
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
				defer server.Close()
				handler := newImageSearchHandler("private_key_canary", server.URL, server.Client(), func(context.Context, string) (llm.InputImage, error) {
					t.Fatal("download on rejected results")
					return llm.InputImage{}, nil
				}, log)
				state := requestctx.NewImageSearchState()
				ctx := requestctx.WithImageSearchState(context.Background(), state)
				_, err := handler(ctx, map[string]interface{}{"query": "private_query_canary", "results": invalid})
				if err == nil || calls != 0 || len(state.ActiveReferences()) != 0 {
					t.Fatalf("err=%v submissions=%d", err, calls)
				}
				var record map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
					t.Fatal("expected one measurement", err)
				}
				if record["event"] != "provider.web.image_search.complete" || record["level"] != "info" || record["status"] != "rejected" || record["is_submitted"] != false || record["attempted_download_count"] != float64(0) || record["requested_result_count"] != nil || strings.Contains(logs.String(), "private_") {
					t.Fatalf("unsafe rejection metrics: %s", logs.String())
				}
			})
		}
	}
}

func TestImageSearchWireInspectionSelectionAndTelemetry(t *testing.T) {
	const canary = "private_image_canary"
	const thumbnail = "https://imgs.search.brave.com/preview?url=https%3A%2F%2Fexample.com%2Fa%3Fx%3D1&width=500"
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/res/v1/images/search" || r.Header.Get("X-Subscription-Token") != canary || r.URL.Query().Get("q") != canary || r.URL.Query().Get("safesearch") != "off" || r.URL.Query().Get("count") != "8" {
					t.Error("wire contract")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "images", "results": []any{
					map[string]any{"title": canary, "url": "https://example.com/page?utm_source=removed", "thumbnail": map[string]string{"src": thumbnail}, "properties": map[string]string{"url": "https://original.invalid/never"}},
				}})
			}))
			defer server.Close()
			var logs bytes.Buffer
			log := config.NewLogger(level)
			log.SetOutput(&logs)
			downloads := 0
			preview := base64.StdEncoding.EncodeToString([]byte("normalized-preview"))
			handler := newImageSearchHandler(canary, server.URL+"/res/v1/images/search", server.Client(), func(_ context.Context, u string) (llm.InputImage, error) {
				downloads++
				if u != thumbnail {
					t.Fatal("thumbnail query altered or original downloaded")
				}
				return llm.InputImage{MimeType: "image/png", Data: preview}, nil
			}, log)
			state := requestctx.NewImageSearchState()
			ctx := requestctx.WithImageSearchState(context.Background(), state)
			ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req_images", OperationID: "op_parent"})
			result, err := handler(ctx, map[string]interface{}{"query": canary})
			if err != nil || downloads != 1 || len(result.Attachments) != 0 || !strings.Contains(result.Content, `"loaded":true`) {
				t.Fatalf("search err=%v", err)
			}
			ref := state.ActiveReferences()[0]
			selectImage := NewImageSelectHandler()
			args := map[string]interface{}{"result_id": ref.ID}
			if _, err := selectImage(ctx, args); err == nil {
				t.Fatal("uninspected selection")
			}
			state.MarkInspected(ref, ref)
			ha := requestctx.WithPrincipal(ctx, identity.Principal{Gateway: "homeassistant"})
			if _, err := selectImage(ha, args); err == nil || len(state.SelectedReferences()) != 0 {
				t.Fatal("HA selection")
			}
			selected, err := selectImage(ctx, args)
			if err != nil || len(selected.Attachments) != 1 || base64.StdEncoding.EncodeToString(selected.Attachments[0].Data) != preview {
				t.Fatal("selection bytes")
			}
			duplicate, err := selectImage(ctx, args)
			if err != nil || len(duplicate.Attachments) != 0 {
				t.Fatal("duplicate attachment")
			}
			if strings.Contains(logs.String(), canary) || strings.Contains(logs.String(), thumbnail) || strings.Contains(logs.String(), preview) {
				t.Fatal("private telemetry")
			}
			var record map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
				t.Fatal("expected exactly one terminal record", err)
			}
			if record["event"] != "provider.web.image_search.complete" || record["level"] != "info" || record["status"] != "ok" || record["request_id"] != "req_images" || record["parent_operation_id"] != "op_parent" || record["image_count"] != float64(1) || record["is_submitted"] != true {
				t.Fatalf("record=%v", record)
			}
			if record["attempted_download_count"] != float64(1) || record["downloaded_image_bytes"] != float64(len("normalized-preview")) {
				t.Fatalf("download metrics=%v", record)
			}
			logs.Reset()
			if _, err := handler(ctx, map[string]interface{}{"query": canary}); err != nil {
				t.Fatal(err)
			}
			if active := state.ActiveReferences(); len(active) != 1 || active[0].ID != ref.ID {
				t.Fatal("cross-search duplicate")
			}
			duplicate, err = selectImage(ctx, args)
			if err != nil || len(duplicate.Attachments) != 0 {
				t.Fatal("cross-search duplicate attachment")
			}
		})
	}
}

func TestImageSearchLoadsTwoPerCallAndStopsAfterTwoCalls(t *testing.T) {
	submissions, downloads := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		submissions++
		results := make([]map[string]any, 12)
		for i := range results {
			results[i] = map[string]any{"title": "synthetic", "url": "https://example.com/page", "thumbnail": map[string]string{"src": fmt.Sprintf("https://example.com/%d.png", i)}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "images", "results": results})
	}))
	defer server.Close()
	handler := newImageSearchHandler("synthetic", server.URL, server.Client(), func(context.Context, string) (llm.InputImage, error) {
		downloads++
		return llm.InputImage{MimeType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("preview-%d", downloads)))}, nil
	}, nil)
	state := requestctx.NewImageSearchState()
	ctx := requestctx.WithImageSearchState(context.Background(), state)
	// Search remains allowed on the text-only gateway; only selection is forbidden.
	ctx = requestctx.WithPrincipal(ctx, identity.Principal{Gateway: "homeassistant"})
	for i := 0; i < 2; i++ {
		if _, err := handler(ctx, map[string]interface{}{"query": "synthetic"}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			ref := state.ActiveReferences()[0]
			state.MarkInspected(ref, ref)
			state.MarkSelected(ref.ID)
		}
	}
	if _, err := handler(ctx, map[string]interface{}{"query": "synthetic"}); err == nil {
		t.Fatal("third execution accepted")
	}
	if submissions != 2 || downloads != 4 {
		t.Fatalf("submissions=%d downloads=%d", submissions, downloads)
	}
	active := state.ActiveReferences()
	if len(active) != 4 || active[0].ID != "search-1" || active[3].ID != "search-4" {
		t.Fatal("active window")
	}
	if selected := state.SelectedReferences(); len(selected) != 1 || selected[0].ID != "search-1" {
		t.Fatal("second call lost first selection")
	}
}

func TestImageSelectionDoesNotStageInvalidAttachment(t *testing.T) {
	state := requestctx.NewImageSearchState()
	refs, err := state.AddReferences([]requestctx.ImageSearchReference{{MIMEType: "image/png", Data: "not base64"}})
	if err != nil {
		t.Fatal(err)
	}
	state.MarkInspected(refs[0], refs[0])
	ctx := requestctx.WithImageSearchState(context.Background(), state)
	if _, err := NewImageSelectHandler()(ctx, map[string]interface{}{"result_id": refs[0].ID}); err == nil {
		t.Fatal("invalid attachment accepted")
	}
	if len(state.SelectedReferences()) != 0 {
		t.Fatal("invalid attachment staged")
	}
}

func TestImageSearchFailureEmptyCancellationAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, body             string
		code                   int
		canceled, failDownload bool
		wantErr                bool
		status                 string
	}{
		{"empty", `{"type":"images","results":[]}`, 200, false, false, false, "ok"},
		{"malformed", `{"type":"images"}`, 200, false, false, true, "error"},
		{"status", `private_error`, 429, false, false, true, "error"},
		{"oversized", strings.Repeat("x", maxResponseBytes+1), 200, false, false, true, "error"},
		{"canceled", ``, 200, true, false, true, "ok"},
		{"download", `{"type":"images","results":[{"title":"test","url":"https://example.com/","thumbnail":{"src":"https://example.com/p.png"}}]}`, 200, false, true, false, "degraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			var logs bytes.Buffer
			log := config.NewLogger(config.LevelInfo)
			log.SetOutput(&logs)
			handler := newImageSearchHandler("synthetic", server.URL, server.Client(), func(context.Context, string) (llm.InputImage, error) {
				return llm.InputImage{}, errors.New("private_download_error")
			}, log)
			ctx, cancel := context.WithCancel(requestctx.WithImageSearchState(context.Background(), requestctx.NewImageSearchState()))
			defer cancel()
			if tc.canceled {
				cancel()
			}
			_, err := handler(ctx, map[string]interface{}{"query": "synthetic"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			if calls > 1 || (tc.canceled && calls != 0) {
				t.Fatal("unexpected submission/retry")
			}
			var record map[string]any
			if json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record) != nil || record["status"] != tc.status {
				t.Fatalf("logs=%s", logs.String())
			}
			if strings.Contains(logs.String(), "private_") {
				t.Fatal("private failure telemetry")
			}
		})
	}
}

func TestImageSearchRejectionTelemetry(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, reason := range []string{"state", "query", "key", "limit", "endpoint"} {
			t.Run(level.String()+"/"+reason, func(t *testing.T) {
				var logs bytes.Buffer
				log := config.NewLogger(level)
				log.SetOutput(&logs)
				state := requestctx.NewImageSearchState()
				ctx := requestctx.WithImageSearchState(context.Background(), state)
				key, query, endpoint := "private_key", "synthetic", "https://example.com/"
				switch reason {
				case "state":
					ctx = context.Background()
				case "query":
					query = ""
				case "key":
					key = ""
				case "limit":
					_ = state.ReserveSearch()
					_ = state.ReserveSearch()
				case "endpoint":
					endpoint = ":private_endpoint"
				}
				handler := newImageSearchHandler(key, endpoint, nil, nil, log)
				if _, err := handler(ctx, map[string]interface{}{"query": query}); err == nil {
					t.Fatal("expected rejection")
				}
				var record map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
					t.Fatal("expected one measurement", err)
				}
				if record["level"] != "info" || record["status"] != "rejected" || record["outcome"] != "rejected" || record["is_submitted"] != false || record["attempted_download_count"] != float64(0) || record["downloaded_image_bytes"] != float64(0) || strings.Contains(logs.String(), "private_") {
					t.Fatalf("rejection metrics: %s", logs.String())
				}
			})
		}
	}
}

func TestImageSearchDownloadCancellationTelemetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"images","results":[{"title":"test","url":"https://example.com/","thumbnail":{"src":"https://example.com/p.png"}}]}`)
	}))
	defer server.Close()
	var logs bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&logs)
	ctx, cancel := context.WithCancel(requestctx.WithImageSearchState(context.Background(), requestctx.NewImageSearchState()))
	defer cancel()
	handler := newImageSearchHandler("synthetic", server.URL, server.Client(), func(ctx context.Context, _ string) (llm.InputImage, error) {
		cancel()
		return llm.InputImage{}, ctx.Err()
	}, log)
	if _, err := handler(ctx, map[string]interface{}{"query": "synthetic"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record["status"] != "ok" || record["outcome"] != "canceled" || record["is_submitted"] != true || record["attempted_download_count"] != float64(1) || record["downloaded_image_bytes"] != float64(0) {
		t.Fatalf("cancellation metrics: %s", logs.String())
	}
}
