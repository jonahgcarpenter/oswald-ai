package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

func TestInfoChatActivityClassificationAndExclusions(t *testing.T) {
	for _, tc := range []struct {
		name, text, inputType      string
		images                     []llm.InputImage
		reply                      *routing.ReplyContext
		command, ignored, rejected bool
	}{
		{name: "text", text: "private-input-canary", inputType: "text"},
		{name: "image", images: []llm.InputImage{{}}, inputType: "image"},
		{name: "mixed", text: "private-input-canary", images: []llm.InputImage{{}}, inputType: "text_image"},
		{name: "replied-image", text: "private-input-canary", reply: &routing.ReplyContext{Images: []llm.InputImage{{MimeType: "image/png", Data: "synthetic"}}}, inputType: "text_image"},
		{name: "command", text: "/help", command: true},
		{name: "ignored", text: "private-input-canary", ignored: true},
		{name: "rejected", text: "private-input-canary", rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, records := telemetryLoggerAtLevel(t, config.LevelInfo)
			b := broker.NewBroker(telemetryProcessor{}, 1, log)
			b.Start()
			defer b.Shutdown()
			principal := testPrincipal("alice")
			if tc.rejected {
				principal.Assurance = ""
			}
			Execute(Request{RequestID: "req", Principal: principal, Text: tc.text, Images: tc.images, Reply: tc.reply, IsGroup: tc.ignored}, Dependencies{Broker: b, Log: log}, &fakeResponder{})
			var info []map[string]any
			for _, record := range records() {
				if record["level"] == "info" {
					info = append(info, record)
				}
			}
			if tc.ignored || tc.rejected {
				if len(info) != 0 {
					t.Fatalf("nonchat activity=%v", info)
				}
				return
			}
			if tc.command {
				if len(info) != 1 || info[0]["event"] != "command.completed" {
					t.Fatalf("command activity=%v", info)
				}
				return
			}
			if len(info) != 2 || info[0]["event"] != "chat.requested" || info[1]["event"] != "chat.completed" {
				t.Fatalf("chat activity=%v", info)
			}
			for _, record := range info {
				if record["input_type"] != tc.inputType || record["request_id"] != "req" || record["user_id"] != "alice" || record["gateway"] != "imessage" {
					t.Fatalf("correlation/classification=%v", record)
				}
				for _, key := range []string{"image_count", "model_image_count", "request_total_tokens", "model", "request_kind"} {
					if _, exists := record[key]; exists {
						t.Fatalf("noisy activity field %s", key)
					}
				}
			}
		})
	}
}

func TestInfoChatCompletionSeparatesExecutionAndDelivery(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		executionErr, deliveryErr error
		execution, delivery       string
	}{
		{"success", nil, nil, "ok", "ok"},
		{"delivery-failure", nil, errors.New("private-error-canary"), "ok", "error"},
		{"model-failure", errors.New("private-error-canary"), nil, "error", "ok"},
		{"canceled", context.Canceled, nil, "canceled", "not_attempted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, records := telemetryLoggerAtLevel(t, config.LevelInfo)
			b := broker.NewBroker(telemetryProcessor{err: tc.executionErr}, 1, log)
			b.Start()
			defer b.Shutdown()
			Execute(Request{RequestID: "req", Principal: testPrincipal("alice"), Text: "hello"}, Dependencies{Broker: b, Log: log}, &fakeResponder{sendErr: tc.deliveryErr})
			count := 0
			for _, record := range records() {
				if record["event"] != "chat.completed" {
					continue
				}
				count++
				if record["execution_outcome"] != tc.execution || record["delivery_outcome"] != tc.delivery {
					t.Fatalf("outcomes=%v", record)
				}
			}
			if count != 1 {
				t.Fatalf("completion count=%d", count)
			}
		})
	}
}
