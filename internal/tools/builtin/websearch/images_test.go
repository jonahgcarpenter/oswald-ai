package websearch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestImageSearchWireInspectionSelectionAndTelemetry(t *testing.T) {
	const canary = "private_image_canary"
	const thumbnail = "https://imgs.search.brave.com/preview?url=https%3A%2F%2Fexample.com%2Fa%3Fx%3D1&width=500"
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/res/v1/images/search" || r.Header.Get("X-Subscription-Token") != canary || r.URL.Query().Get("q") != canary || r.URL.Query().Get("safesearch") != "strict" || r.URL.Query().Get("count") != "8" {
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
	}
	if _, err := handler(ctx, map[string]interface{}{"query": "synthetic"}); err == nil {
		t.Fatal("third execution accepted")
	}
	if submissions != 2 || downloads != 4 {
		t.Fatalf("submissions=%d downloads=%d", submissions, downloads)
	}
	active := state.ActiveReferences()
	if len(active) != 2 || active[0].ID != "search-3" || active[1].ID != "search-4" {
		t.Fatal("active window")
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
