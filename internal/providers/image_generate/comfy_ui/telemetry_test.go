package comfy_ui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func logDetails(t *testing.T, record map[string]any) map[string]any {
	t.Helper()
	details, ok := record["details"].(map[string]any)
	if !ok {
		t.Fatalf("missing details object: %#v", record)
	}
	return details
}

func TestGenerationStagesAndCleanupWarningExcludeProviderProse(t *testing.T) {
	const canary = "private_generation_prose"
	imageData := testPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prompt":
			_, _ = io.WriteString(w, `{"prompt_id":"`+canary+`","unknown":"`+canary+`"}`)
		case "/history/" + canary:
			_, _ = io.WriteString(w, `{"`+canary+`":{"outputs":{"10":{"images":[{"filename":"`+canary+`.png","type":"output"}]}}}}`)
		case "/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(imageData)
		case "/free":
			http.Error(w, canary, http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := NewWorkflow(TextToImage)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: "req_image", OperationID: "op_parent"})
	result, err := client.Generate(ctx, log, workflow, canary, canary, nil, nil, "landscape")
	if err != nil || !result.CleanupFailed {
		t.Fatalf("err=%v degraded=%t", err, result.CleanupFailed)
	}
	stages := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r["event"] == "provider.comfyui.stage.complete" {
			d := logDetails(t, r)
			stages[d["phase"].(string)]++
			if r["level"] != "debug" || d["duration_ms"] == nil || d["parent_operation_id"] != "op_parent" {
				t.Errorf("stage=%+v", r)
			}
			if d["phase"] == "cleanup" && (d["http_status"] != float64(503) || d["error_code"] != "http_server_error") {
				t.Errorf("cleanup=%+v", r)
			}
		}
	}
	for _, phase := range []string{"permit", "submit", "poll", "download", "cleanup"} {
		if stages[phase] != 1 {
			t.Errorf("stages=%v", stages)
		}
	}
	if strings.Contains(output.String(), canary) {
		t.Fatalf("logs=%s", output.String())
	}
}

func TestImageStrengthStageTelemetry(t *testing.T) {
	const canary = "private_strength_prompt"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/image":
			_ = json.NewEncoder(w).Encode(map[string]string{"name": InputFilename, "subfolder": InputSubfolder, "type": "input"})
		case "/prompt":
			http.Error(w, canary, http.StatusBadRequest)
		case "/free":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	for _, level := range []config.Level{config.LevelDebug} {
		for _, explicit := range []bool{false, true} {
			client, err := NewClient(server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			workflow, err := NewWorkflow(ImageToImage)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			log := config.NewLogger(level)
			log.SetOutput(&output)
			ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: "req_strength", OperationID: "op_strength"})
			want := interface{}(0.75)
			var strength *float64
			if explicit {
				value := 0.6
				strength = &value
				want = 0.6
			}
			result, err := client.Generate(ctx, log, workflow, canary, "", strength, testPNG(t), "landscape")
			if err == nil {
				t.Fatal("expected submission failure")
			}
			if result.EffectiveStrength == nil || *result.EffectiveStrength != want {
				t.Fatalf("effective strength = %v, want %v", result.EffectiveStrength, want)
			}
			submits := 0
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				var record map[string]interface{}
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["event"] == "provider.comfyui.stage.complete" && logDetails(t, record)["phase"] == "submit" {
					submits++
					d := logDetails(t, record)
					if d["strength"] != want || record["level"] != "debug" || d["status"] != "error" || d["request_id"] != "req_strength" || d["parent_operation_id"] != "op_strength" {
						t.Fatalf("unexpected strength measurement: %+v", record)
					}
				}
			}
			if submits != 1 || strings.Contains(output.String(), canary) {
				t.Fatalf("unsafe or missing measurement: %s", output.String())
			}
		}
	}
}
