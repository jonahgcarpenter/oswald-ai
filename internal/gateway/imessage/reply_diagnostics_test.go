package imessage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestIgnoredReplyRoutingDiagnostics(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, tc := range []struct {
			name, reason   string
			reply, command bool
		}{
			{"no reference", "group_message_without_invocation", false, false},
			{"human reply", "group_message_without_invocation", true, false},
			{"command reply", "group_command_without_mention", true, true},
		} {
			t.Run(fmt.Sprintf("%s/level_%d", tc.name, level), func(t *testing.T) {
				log, events := captureInfoSummaries(t, level)
				g := &Gateway{Log: log, messageIndex: make(map[string]messageContext)}
				msg := webhookMessage{Text: "private-body", Handle: messageHandle{Address: "private-address"},
					Chats: []messageChat{{GUID: "private-chat", Style: chatStyleGroup}}}
				if tc.reply {
					msg.ReplyToGUID = "private-attachment"
					g.rememberMessage(msg.ReplyToGUID, messageContext{ChatGUID: "private-chat", Text: "private-body", CreatedAt: time.Now()})
				}
				if tc.command {
					msg.Text = "/help"
				}
				g.processReceivedMessage(msg, "req-routing-debug", time.Now())
				counts := map[string]int{}
				for _, event := range events() {
					name := event["event"].(string)
					counts[name]++
					if name != "gateway.message.routing" && name != "gateway.message.ignored" {
						continue
					}
					if event["level"] != "debug" || event["request_id"] != "req-routing-debug" || event["reason_code"] != tc.reason ||
						event["is_reply_to_bot"] != false || event["has_reply_to_guid"] != tc.reply || event["has_thread_originator_guid"] != false {
						t.Fatalf("routing diagnostic=%+v", event)
					}
					if name == "gateway.message.routing" && (event["is_reply_found"] != (tc.reply && !tc.command) || event["is_reply_lookup_attempted"] != (tc.reply && !tc.command)) {
						t.Fatalf("lookup flags=%+v", event)
					}
				}
				want := 0
				if level == config.LevelDebug {
					want = 1
				}
				if counts["gateway.message.routing"] != want || counts["gateway.message.ignored"] != want {
					t.Fatalf("counts=%v", counts)
				}
			})
		}
	}
}

func TestReplyEligibilityDiagnostics(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, reason := range []string{"eligible_bot", "human_authored", "send_error_missing", "send_failed", "message_corrupt", "message_retracted", "no_usable_content"} {
			t.Run(fmt.Sprintf("%s/level_%d", reason, level), func(t *testing.T) {
				log, events := captureInfoSummaries(t, level)
				zero, failed, retracted := 0, 1, int64(1)
				no := false
				data := messageLookupData{GUID: "private-attachment", Text: "private-body", IsFromMe: true,
					SendError: &zero, IsSystemMessage: &no, IsServiceMessage: &no,
					Chats: []messageChat{{GUID: "private-chat"}}}
				switch reason {
				case "human_authored":
					data.IsFromMe = false
				case "send_error_missing":
					data.SendError = nil
				case "send_failed":
					data.SendError = &failed
				case "message_corrupt":
					data.IsCorrupt = true
				case "message_retracted":
					data.DateRetracted = &retracted
				case "no_usable_content":
					data.Text = ""
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewEncoder(w).Encode(messageQueryResponse{Data: []messageLookupData{data}})
				}))
				defer server.Close()
				g := &Gateway{Log: log, BlueBubblesURL: server.URL, BlueBubblesPassword: "private-address", HTTPClient: server.Client()}
				result, found := g.resolveReply(context.Background(), webhookMessage{ReplyToGUID: data.GUID,
					Chats: data.Chats}, true, "req-eligibility-debug")
				if !found || result.IsFromBot != (reason == "eligible_bot") {
					t.Fatalf("unexpected recognition found=%t bot=%t", found, result.IsFromBot)
				}
				infoCount, debugCount := 0, 0
				for _, event := range events() {
					if event["event"] == "gateway.reply_lookup.complete" {
						infoCount++
					}
					if event["event"] == "gateway.reply_lookup.decision" {
						debugCount++
						if event["level"] != "debug" || event["reason_code"] != reason || event["bot_eligibility_reason"] != reason || event["request_id"] != "req-eligibility-debug" || event["is_reply_to_bot"] != result.IsFromBot {
							t.Fatalf("eligibility diagnostic=%+v", event)
						}
					}
				}
				wantDebug := 0
				if level == config.LevelDebug {
					wantDebug = 1
				}
				if infoCount != 1 || debugCount != wantDebug {
					t.Fatalf("info=%d debug=%d", infoCount, debugCount)
				}
			})
		}
	}
}

func TestReplyHTTPDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		status             int
		cancel, timeout    bool
	}{
		{name: "success", body: `{}`, reason: "resolved", status: 200},
		{name: "http failure", body: "private-body", reason: "http_status_failed", status: 503},
		{name: "malformed", body: "private-body", reason: "response_malformed", status: 200},
		{name: "oversized", body: strings.Repeat("x", replyLookupBodyLimit+1), reason: "body_too_large", status: 200},
		{name: "canceled", reason: "lookup_canceled", cancel: true},
		{name: "timeout", reason: "lookup_timeout", timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, events := captureInfoSummaries(t, config.LevelDebug)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			g := &Gateway{Log: log, HTTPClient: server.Client()}
			ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{RequestID: "req-http-debug"})
			if tc.cancel {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			} else if tc.timeout {
				expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				ctx = expired
			}
			var response messageLookupResponse
			_ = g.replyLookupHTTP(ctx, http.MethodGet, server.URL+"/private-chat?password=private-address", nil, &response)
			got := events()
			if len(got) != 1 || got[0]["event"] != "gateway.reply_lookup.http.complete" || got[0]["level"] != "debug" || got[0]["reason_code"] != tc.reason || got[0]["request_id"] != "req-http-debug" {
				t.Fatalf("HTTP diagnostic=%+v", got)
			}
			if _, ok := got[0]["http_status"].(float64); !ok {
				t.Fatalf("http_status must be numeric: %+v", got)
			}
		})
	}
}
