package imessage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func replyFixture(guid string, row int64, bot bool) messageLookupData {
	date, zero, no := int64(1700000000000), 0, false
	return messageLookupData{
		GUID: guid, OriginalROWID: row, DateCreated: &date, Text: "private-body " + guid,
		IsFromMe: bot, SendError: &zero, IsSystemMessage: &no, IsServiceMessage: &no,
		Handle: messageHandle{Address: "+15551234567"}, Chats: []messageChat{{GUID: "private-chat;+;group", Style: chatStyleGroup}},
	}
}

func threadIncoming() webhookMessage {
	return webhookMessage{
		GUID: "incoming", Text: "follow up", ThreadOriginatorGUID: "root", ThreadOriginatorPart: "0:0",
		Handle: messageHandle{Address: "+15551234567"}, Chats: []messageChat{{GUID: "private-chat;+;group", Style: chatStyleGroup}},
	}
}

// This fake executes the shipped predicate against raw SQLite dates. It also
// models BlueBubbles' independently bound chatGuid parameter and response shape.
func sqliteReplyLookup(t *testing.T, messages []messageLookupData, dates map[string]int64) http.HandlerFunc {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE message (ROWID INTEGER PRIMARY KEY, guid TEXT, date INTEGER, thread_originator_guid TEXT, thread_originator_part TEXT, associated_message_type INTEGER, is_system_message INTEGER, is_service_message INTEGER, item_type INTEGER);
CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, guid TEXT);
CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);`)
	if err != nil {
		t.Fatal(err)
	}
	byGUID := make(map[string]messageLookupData)
	chatID := 0
	for _, m := range messages {
		byGUID[m.GUID] = m
		var root, part any
		if m.ThreadOriginatorGUID != "" {
			root = m.ThreadOriginatorGUID
		}
		if m.ThreadOriginatorPart != "" {
			part = m.ThreadOriginatorPart
		}
		typ := strings.Trim(string(m.AssociatedMessageType), "\"")
		if typ == "" {
			typ = "0"
		}
		_, err = db.Exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, m.OriginalROWID, m.GUID, dates[m.GUID], root, part, typ, m.IsSystemMessage, m.IsServiceMessage, m.ItemType)
		if err != nil {
			t.Fatal(err)
		}
		for _, chat := range m.Chats {
			chatID++
			_, err = db.Exec(`INSERT INTO chat VALUES (?, ?); INSERT INTO chat_message_join VALUES (?, ?)`, chatID, chat.GUID, chatID, m.OriginalROWID)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(messageLookupResponse{Data: byGUID[strings.TrimPrefix(r.URL.Path, "/api/v1/message/")]})
			return
		}
		var q messageQueryRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if q.Limit != 2 || q.Offset != 0 || len(q.Where) != 1 || q.ChatGUID == "" {
			t.Errorf("unbounded or unscoped query: %+v", q)
			w.WriteHeader(400)
			return
		}
		args := []any{sql.Named("guid", q.ChatGUID)}
		for k, v := range q.Where[0].Args {
			if k == "guid" {
				t.Error("query overwrites BlueBubbles chat parameter")
			}
			args = append(args, sql.Named(k, v))
		}
		rows, err := db.Query(`SELECT message.guid FROM message JOIN chat_message_join cj ON cj.message_id = message.ROWID JOIN chat ON chat.ROWID = cj.chat_id WHERE chat.guid = :guid AND (`+q.Where[0].Statement+`) ORDER BY message.date DESC LIMIT 2`, args...)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		var result []messageLookupData
		for rows.Next() {
			var guid string
			if err := rows.Scan(&guid); err != nil {
				t.Error(err)
			}
			result = append(result, byGUID[guid])
		}
		if err := rows.Err(); err != nil {
			t.Error(err)
		}
		_ = rows.Close()
		_ = json.NewEncoder(w).Encode(messageQueryResponse{Data: result})
	}
}

func TestThreadPredecessorAdmissionAndOutboundThreading(t *testing.T) {
	for _, name := range []string{"cold without delivery receipts", "warm human root", "intervening human", "future human", "tie human", "tie bot", "raw nanoseconds", "missing raw date", "root only", "other chat", "other thread", "other part", "reaction", "service", "system", "attachment bot", "attachment human", "failed bot"} {
		t.Run(name, func(t *testing.T) {
			root, bot, incoming := replyFixture("root", 1, false), replyFixture("bot", 2, true), replyFixture("incoming", 10, false)
			bot.ThreadOriginatorGUID, incoming.ThreadOriginatorGUID = "root", "root"
			bot.ThreadOriginatorPart, incoming.ThreadOriginatorPart = "0:0", "0:0"
			extra := replyFixture("extra", 3, false)
			extra.ThreadOriginatorGUID, extra.ThreadOriginatorPart = "root", "0:0"
			const raw int64 = 800000000000000000
			dates := map[string]int64{"root": raw, "bot": raw + 10, "extra": raw + 20, "incoming": raw + 30}
			want := true
			messages := []messageLookupData{root, bot, incoming}
			switch name {
			case "intervening human":
				messages = append(messages, extra)
				want = false
			case "future human":
				dates["extra"] = raw + 40
				messages = append(messages, extra)
			case "tie human":
				dates["bot"], dates["extra"], dates["incoming"] = raw+30, raw+30, raw+30
				messages = append(messages, extra)
				want = false
			case "tie bot":
				extra.OriginalROWID = 11
				dates["bot"], dates["extra"], dates["incoming"] = raw+30, raw+30, raw+30
				messages = append(messages, extra)
			case "raw nanoseconds":
				extra.OriginalROWID = 9
				dates["extra"] = raw + 9
				messages = append(messages, extra)
			case "missing raw date":
				dates["extra"] = 0
				messages = append(messages, extra)
				want = false
			case "root only":
				messages = []messageLookupData{root, incoming}
				want = false
			case "other chat":
				extra.Chats[0].GUID = "other-chat"
				messages = append(messages, extra)
			case "other thread":
				extra.ThreadOriginatorGUID = "other-root"
				messages = append(messages, extra)
			case "other part":
				extra.ThreadOriginatorPart = "1:0"
				messages = append(messages, extra)
			case "reaction":
				extra.AssociatedMessageType = json.RawMessage(`2001`)
				messages = append(messages, extra)
			case "service":
				yes := true
				extra.IsServiceMessage = &yes
				messages = append(messages, extra)
			case "system":
				yes := true
				extra.IsSystemMessage = &yes
				messages = append(messages, extra)
			case "attachment bot":
				messages[1].Text = ""
				messages[1].Attachments = []attachment{{GUID: "private-attachment", MimeType: "application/pdf", TransferName: "private-filename.pdf"}}
			case "attachment human":
				extra.Text = ""
				extra.Attachments = []attachment{{GUID: "private-attachment", MimeType: "application/pdf"}}
				messages = append(messages, extra)
				want = false
			case "failed bot":
				code := 1
				messages[1].SendError = &code
				want = false
			}
			bb := newFakeBlueBubbles(t)
			defer bb.server.Close()
			bb.mu.Lock()
			bb.messageLookup = sqliteReplyLookup(t, messages, dates)
			bb.mu.Unlock()
			g, b, model := newIMessageTestGateway(t, bb.server.URL)
			defer b.Shutdown()
			if name == "warm human root" {
				g.rememberInboundMessage(webhookMessage{GUID: "root", Text: "cached root", Chats: root.Chats}, "unused", "human", "Human")
			}
			g.processIncomingMessage(threadIncoming())
			requests := model.primaryRequests()
			if (len(requests) == 1) != want {
				t.Fatalf("requests=%d want admitted=%v", len(requests), want)
			}
			if !want {
				for _, path := range bb.paths() {
					if !strings.HasPrefix(path, "/api/v1/message/") {
						t.Fatalf("ignored request had side effect: %s", path)
					}
				}
				if len(bb.sentMessages()) != 0 {
					t.Fatal("ignored message sent output")
				}
				return
			}
			prompt := requests[0].Messages[len(requests[0].Messages)-1].Content
			if !strings.Contains(prompt, "preceding thread message, not necessarily the selected bubble") || !strings.Contains(prompt, "follow up") {
				t.Fatalf("prompt=%s", prompt)
			}
			if model.lastPrincipal().ExternalID != "+15551234567" {
				t.Fatal("predecessor changed owner")
			}
			sent := bb.sentMessages()
			if len(sent) != 1 || sent[0].SelectedMessageGUID != "incoming" || sent[0].Method != "private-api" || sent[0].PartIndex != 0 {
				t.Fatalf("outbound changed: %+v", sent)
			}
			if !bb.waitForPath("/api/v1/chat/private-chat%3B+%3Bgroup/typing") {
				t.Fatal("indicator did not finish")
			}
		})
	}
}

func TestRESTExplicitReplyTargetAdmissionAndThreading(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, mode := range []string{"bot", "human", "failed bot", "missing error", "corrupt", "retracted", "empty", "wrong chat", "wrong guid", "missing flags", "system", "missing", "lookup failure"} {
			t.Run(level.String()+"/"+mode, func(t *testing.T) {
				log, events := captureInfoSummaries(t, level)
				root := replyFixture("private-attachment-root", 1, false)
				anchor := replyFixture("private-attachment-incoming", 10, false)
				target := replyFixture("private-attachment-target", 3, true)
				prior := replyFixture("private-attachment-prior", 9, true)
				anchor.ThreadOriginatorGUID, anchor.ThreadOriginatorPart, anchor.ReplyToGUID = root.GUID, "0:0", target.GUID
				prior.ThreadOriginatorGUID, prior.ThreadOriginatorPart = root.GUID, "0:0"
				status, reason, direct, rejected, missing, failed := "ok", "not_eligible_bot", float64(1), float64(0), float64(0), float64(0)
				switch mode {
				case "bot":
					reason = "resolved"
				case "human":
					target.IsFromMe = false
				case "failed bot":
					code := 1
					target.SendError = &code
				case "missing error":
					target.SendError = nil
				case "corrupt":
					target.IsCorrupt = true
				case "retracted":
					date := int64(1)
					target.DateRetracted = &date
				case "empty":
					target.Text = ""
				case "wrong chat":
					target.Chats[0].GUID = "private-chat-wrong"
					status, reason, direct, rejected = "rejected", "message_chat_mismatch", 0, 1
				case "wrong guid":
					target.GUID = "private-attachment-wrong"
					status, reason, direct, rejected = "rejected", "message_guid_mismatch", 0, 1
				case "missing flags":
					target.IsServiceMessage = nil
					status, reason, direct, rejected = "rejected", "message_flags_missing", 0, 1
				case "system":
					yes := true
					target.IsSystemMessage = &yes
					status, reason, direct, rejected = "rejected", "non_conversational_message", 0, 1
				case "missing":
					reason, direct, missing = "message_not_found", 0, 1
				case "lookup failure":
					status, reason, direct, failed = "error", "lookup_failed", 0, 1
				}
				bb := newFakeBlueBubbles(t)
				defer bb.server.Close()
				var queries, gets, predecessors atomic.Int32
				bb.mu.Lock()
				bb.messageLookup = func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						gets.Add(1)
						if r.URL.Path != "/api/v1/message/"+anchor.ReplyToGUID {
							t.Error("GET fallback did not look up explicit target")
						}
						if mode == "lookup failure" {
							w.WriteHeader(http.StatusServiceUnavailable)
						} else {
							_ = json.NewEncoder(w).Encode(messageLookupResponse{})
						}
						return
					}
					var q messageQueryRequest
					if err := json.NewDecoder(r.Body).Decode(&q); err != nil || len(q.Where) != 1 {
						t.Error("invalid lookup query")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if q.ChatGUID != root.Chats[0].GUID || q.Limit != 2 || q.Offset != 0 {
						t.Error("lookup lost exact chat or bounds")
					}
					if q.Where[0].Statement != "message.guid = :reply_guid" {
						predecessors.Add(1)
						// A prior bot is available, but must never replace the explicit target.
						_ = json.NewEncoder(w).Encode(messageQueryResponse{Data: []messageLookupData{prior}})
						return
					}
					step := queries.Add(1)
					wantGUID := root.GUID
					row := root
					if step == 2 {
						wantGUID, row = anchor.GUID, anchor
					} else if step == 3 {
						wantGUID, row = anchor.ReplyToGUID, target
					} else if step != 1 {
						t.Error("repeated direct lookup")
					}
					if q.Where[0].Args["reply_guid"] != wantGUID {
						t.Error("unexpected direct lookup order or target")
					}
					if step == 3 && mode == "lookup failure" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					rows := []messageLookupData{row}
					if step == 3 && mode == "missing" {
						rows = nil
					}
					_ = json.NewEncoder(w).Encode(messageQueryResponse{Data: rows})
				}
				bb.mu.Unlock()
				g, b, model := newIMessageTestGateway(t, bb.server.URL)
				defer b.Shutdown()
				g.Log = log
				msg := threadIncoming()
				msg.GUID, msg.ThreadOriginatorGUID = anchor.GUID, root.GUID
				if mode != "bot" {
					// Rejection must precede account resolution and attachment downloads.
					g.Links = nil
					msg.Attachments = []attachment{{GUID: "private-attachment-input", MimeType: "image/png"}}
				}
				g.processIncomingMessage(msg)
				wantGets := int32(0)
				if mode == "missing" || mode == "lookup failure" {
					wantGets = 1
				}
				if queries.Load() != 3 || gets.Load() != wantGets || predecessors.Load() != 0 {
					t.Fatalf("queries=%d gets=%d predecessors=%d", queries.Load(), gets.Load(), predecessors.Load())
				}
				requests := model.primaryRequests()
				if mode == "bot" {
					if len(requests) != 1 {
						t.Fatalf("model invocations=%d", len(requests))
					}
					prompt := requests[0].Messages[len(requests[0].Messages)-1].Content
					if !strings.Contains(prompt, target.Text) || !strings.Contains(prompt, msg.Text) || strings.Contains(prompt, "preceding thread message") || strings.Contains(prompt, root.Text) || strings.Contains(prompt, prior.Text) {
						t.Fatal("explicit target was not enriched as a direct reply")
					}
					if model.lastPrincipal().ExternalID != msg.Handle.Address {
						t.Fatal("explicit reply changed ownership")
					}
					sent := bb.sentMessages()
					if len(sent) != 1 || sent[0].SelectedMessageGUID != msg.GUID || sent[0].Method != "private-api" || sent[0].PartIndex != 0 || sent[0].ChatGUID != msg.primaryChat().GUID {
						t.Fatalf("outbound threading changed: %+v", sent)
					}
					if !bb.waitForPath("/api/v1/chat/private-chat%3B+%3Bgroup/typing") {
						t.Fatal("indicator did not finish")
					}
				} else {
					if len(requests) != 0 || len(bb.sentMessages()) != 0 {
						t.Fatal("ineligible explicit target invoked model or sent output")
					}
					for _, path := range bb.paths() {
						if path != "/api/v1/message/query" && path != "/api/v1/message/"+anchor.ReplyToGUID {
							t.Fatalf("rejected reply had side effect: %s", path)
						}
					}
				}
				count := 0
				for _, event := range events() {
					if event["event"] != "gateway.reply_lookup.complete" {
						continue
					}
					count++
					if event["level"] != "info" || event["record_kind"] != "measurement" || event["phase"] != "direct_target" || event["reason_code"] != reason || event["status"] != status || event["outcome"] != "complete" || event["remote_count"] != float64(3) || event["direct_count"] != direct || event["predecessor_count"] != float64(0) || event["rejected_count"] != rejected || event["not_found_count"] != missing || event["error_count"] != failed {
						t.Fatalf("measurement=%+v", event)
					}
				}
				if count != 1 {
					t.Fatalf("measurement count=%d", count)
				}
			})
		}
	}
}

func TestReplyResolverAnchorRejectionMeasurements(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, tc := range []struct {
			name, reason string
			change       func(*messageLookupData)
		}{
			{"zero row", "anchor_row_missing", func(m *messageLookupData) { m.OriginalROWID = 0 }},
			{"negative row", "anchor_row_missing", func(m *messageLookupData) { m.OriginalROWID = -1 }},
			{"absent date", "anchor_date_missing", func(m *messageLookupData) { m.DateCreated = nil }},
			{"zero date", "anchor_date_missing", func(m *messageLookupData) { *m.DateCreated = 0 }},
			{"negative date", "anchor_date_missing", func(m *messageLookupData) { *m.DateCreated = -1 }},
			{"self authored", "anchor_self_authored", func(m *messageLookupData) { m.IsFromMe = true }},
			{"missing system flag", "message_flags_missing", func(m *messageLookupData) { m.IsSystemMessage = nil }},
			{"missing service flag", "message_flags_missing", func(m *messageLookupData) { m.IsServiceMessage = nil }},
			{"system", "non_conversational_message", func(m *messageLookupData) { *m.IsSystemMessage = true }},
			{"service", "non_conversational_message", func(m *messageLookupData) { *m.IsServiceMessage = true }},
			{"reaction", "non_conversational_message", func(m *messageLookupData) { m.AssociatedMessageType = json.RawMessage(`2001`) }},
			{"item type", "non_conversational_message", func(m *messageLookupData) { m.ItemType = 1 }},
			{"wrong guid", "message_guid_mismatch", func(m *messageLookupData) { m.GUID = "private-attachment-wrong" }},
			{"wrong chat", "message_chat_mismatch", func(m *messageLookupData) { m.Chats[0].GUID = "private-chat-wrong" }},
			{"missing chat", "message_chat_mismatch", func(m *messageLookupData) { m.Chats = nil }},
			{"wrong thread", "anchor_thread_mismatch", func(m *messageLookupData) { m.ThreadOriginatorGUID = "private-attachment-other-root" }},
			{"missing thread", "anchor_thread_mismatch", func(m *messageLookupData) { m.ThreadOriginatorGUID = "" }},
			{"wrong part", "anchor_part_mismatch", func(m *messageLookupData) { m.ThreadOriginatorPart = "1:0" }},
			{"missing part", "anchor_part_mismatch", func(m *messageLookupData) { m.ThreadOriginatorPart = "" }},
		} {
			t.Run(level.String()+"/"+tc.name, func(t *testing.T) {
				log, events := captureInfoSummaries(t, level)
				root, anchor := replyFixture("private-attachment-root", 1, false), replyFixture("private-attachment-incoming", 10, false)
				msg := threadIncoming()
				msg.GUID, msg.ThreadOriginatorGUID = anchor.GUID, root.GUID
				anchor.ThreadOriginatorGUID, anchor.ThreadOriginatorPart = root.GUID, msg.ThreadOriginatorPart
				anchor.ReplyToGUID = "private-attachment-explicit-bot"
				tc.change(&anchor)
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					step := calls.Add(1)
					var q messageQueryRequest
					if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&q) != nil || len(q.Where) != 1 || step > 2 {
						t.Error("anchor rejection performed unexpected lookup")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					row, guid := root, root.GUID
					if step == 2 {
						row, guid = anchor, msg.GUID
					}
					if q.ChatGUID != msg.primaryChat().GUID || q.Limit != 2 || q.Where[0].Statement != "message.guid = :reply_guid" || q.Where[0].Args["reply_guid"] != guid {
						t.Error("lookup did not validate root then exact-chat anchor")
					}
					_ = json.NewEncoder(w).Encode(messageQueryResponse{Data: []messageLookupData{row}})
				}))
				defer server.Close()
				g := &Gateway{BlueBubblesURL: server.URL, Log: log}
				result, found := g.resolveReply(context.Background(), msg, true, "req-anchor-rejection")
				if !found || result.IsFromBot || result.IsPredecessor || result.Text != root.Text || calls.Load() != 2 {
					t.Fatal("invalid anchor did not stop after the validated human root")
				}
				got := events()
				if len(got) != 1 {
					t.Fatalf("measurement count=%d", len(got))
				}
				event := got[0]
				if event["event"] != "gateway.reply_lookup.complete" || event["level"] != "info" || event["record_kind"] != "measurement" || event["request_id"] != "req-anchor-rejection" || event["phase"] != "anchor" || event["reason_code"] != tc.reason || event["status"] != "rejected" || event["outcome"] != "complete" || event["remote_count"] != float64(2) || event["rejected_count"] != float64(1) || event["cache_count"] != float64(0) || event["direct_count"] != float64(0) || event["predecessor_count"] != float64(0) || event["not_found_count"] != float64(0) || event["error_count"] != float64(0) {
					t.Fatalf("measurement=%+v", event)
				}
				if duration, ok := event["duration_ms"].(float64); !ok || duration < 0 {
					t.Fatal("missing nonnegative numeric duration")
				}
			})
		}
	}
}

func TestReplyResolverRejectsUnvalidatedMetadata(t *testing.T) {
	for _, name := range []string{"wrong root guid", "wrong root chat", "missing root chat", "wrong anchor guid", "wrong anchor chat", "missing anchor chat", "anchor absent", "anchor row zero", "anchor row negative", "anchor date absent", "anchor thread", "anchor part", "anchor target", "candidate chat", "candidate missing chat", "candidate thread", "candidate part", "candidate part with empty anchor", "candidate date absent", "candidate row zero", "candidate future", "candidate ambiguous", "candidate anchor", "candidate reaction", "candidate corrupt", "candidate retracted", "explicit human", "no thread"} {
		t.Run(name, func(t *testing.T) {
			root, anchor, bot := replyFixture("root", 1, false), replyFixture("incoming", 10, false), replyFixture("bot", 2, true)
			anchor.ThreadOriginatorGUID, bot.ThreadOriginatorGUID = "root", "root"
			anchor.ThreadOriginatorPart, bot.ThreadOriginatorPart = "0:0", "0:0"
			msg := threadIncoming()
			switch name {
			case "wrong root guid":
				root.GUID = "wrong"
			case "wrong root chat":
				root.Chats[0].GUID = "wrong"
			case "missing root chat":
				root.Chats = nil
			case "wrong anchor guid":
				anchor.GUID = "wrong"
			case "wrong anchor chat":
				anchor.Chats[0].GUID = "wrong"
			case "missing anchor chat":
				anchor.Chats = nil
			case "anchor absent":
				anchor = messageLookupData{}
			case "anchor row zero":
				anchor.OriginalROWID = 0
			case "anchor row negative":
				anchor.OriginalROWID = -1
			case "anchor date absent":
				anchor.DateCreated = nil
			case "anchor thread":
				anchor.ThreadOriginatorGUID = "wrong"
			case "anchor part":
				anchor.ThreadOriginatorPart = "wrong"
			case "anchor target":
				anchor.ReplyToGUID = "human"
			case "candidate chat":
				bot.Chats[0].GUID = "wrong"
			case "candidate missing chat":
				bot.Chats = nil
			case "candidate thread":
				bot.ThreadOriginatorGUID = "wrong"
			case "candidate part":
				bot.ThreadOriginatorPart = "wrong"
			case "candidate part with empty anchor":
				msg.ThreadOriginatorPart, anchor.ThreadOriginatorPart = "", ""
			case "candidate date absent":
				bot.DateCreated = nil
			case "candidate row zero":
				bot.OriginalROWID = 0
			case "candidate future":
				date := *anchor.DateCreated + 1
				bot.DateCreated = &date
			case "candidate anchor":
				bot.GUID = "incoming"
			case "candidate reaction":
				bot.AssociatedMessageType = json.RawMessage(`"love"`)
			case "candidate corrupt":
				bot.IsCorrupt = true
			case "candidate retracted":
				date := int64(1)
				bot.DateRetracted = &date
			case "explicit human":
				msg.ReplyToGUID = "human"
			case "no thread":
				msg.ThreadOriginatorGUID = ""
				msg.ReplyToGUID = "root"
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(messageLookupResponse{})
					return
				}
				var q messageQueryRequest
				_ = json.NewDecoder(r.Body).Decode(&q)
				var result []messageLookupData
				switch q.Where[0].Args["reply_guid"] {
				case "root":
					result = []messageLookupData{root}
				case "incoming":
					result = []messageLookupData{anchor}
				case "human":
					human := replyFixture("human", 4, false)
					result = []messageLookupData{human}
				default:
					result = []messageLookupData{bot}
					if name == "candidate ambiguous" {
						result = append(result, bot)
					}
				}
				_ = json.NewEncoder(w).Encode(messageQueryResponse{Data: result})
			}))
			defer server.Close()
			// Nil account/media services make any preflight side effect a failure.
			g := &Gateway{BlueBubblesURL: server.URL, Log: config.NewLogger(config.LevelError)}
			g.processIncomingMessage(msg)
			if calls.Load() > 3 {
				t.Fatalf("duplicate or unbounded lookup: %d", calls.Load())
			}
		})
	}
}

func TestDirectReplyCacheAndRESTScope(t *testing.T) {
	for _, mode := range []string{"cache", "query without delivery receipts", "get", "second matching chat", "missing chats", "wrong cache chat"} {
		t.Run(mode, func(t *testing.T) {
			bot := replyFixture("bot", 1, true)
			if mode == "second matching chat" {
				bot.Chats = append([]messageChat{{GUID: "other-chat", Style: chatStyleGroup}}, bot.Chats...)
			}
			if mode == "missing chats" {
				bot.Chats = nil
			}
			bb := newFakeBlueBubbles(t)
			defer bb.server.Close()
			bb.mu.Lock()
			bb.messageLookup = func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(messageLookupResponse{Data: bot})
					return
				}
				rows := []messageLookupData{bot}
				if mode == "get" {
					rows = nil
				}
				if mode == "wrong cache chat" {
					rows = []messageLookupData{{}}
				}
				_ = json.NewEncoder(w).Encode(messageQueryResponse{Data: rows})
			}
			bb.mu.Unlock()
			g, b, model := newIMessageTestGateway(t, bb.server.URL)
			defer b.Shutdown()
			if mode == "cache" || mode == "wrong cache chat" {
				chat := bot.Chats[0].GUID
				if mode == "wrong cache chat" {
					chat = "wrong"
				}
				g.rememberBotMessage("bot", "unused", chat, "unused", "answer")
			}
			msg := threadIncoming()
			msg.ThreadOriginatorGUID = ""
			msg.ReplyToGUID = "bot"
			g.processIncomingMessage(msg)
			want := mode != "wrong cache chat" && mode != "missing chats"
			if (len(model.primaryRequests()) == 1) != want {
				t.Fatal("incorrect direct admission")
			}
			if want && !bb.waitForPath("/api/v1/chat/private-chat%3B+%3Bgroup/typing") {
				t.Fatal("indicator did not finish")
			}
		})
	}
}

func TestReplyLookupFailureDoesNotBlockMentionOrDM(t *testing.T) {
	for _, mode := range []string{"dm", "mention", "group command"} {
		t.Run(mode, func(t *testing.T) {
			bb := newFakeBlueBubbles(t)
			defer bb.server.Close()
			var calls atomic.Int32
			bb.mu.Lock()
			bb.messageLookup = func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }
			bb.mu.Unlock()
			g, b, model := newIMessageTestGateway(t, bb.server.URL)
			defer b.Shutdown()
			msg := threadIncoming()
			switch mode {
			case "dm":
				msg.Chats = []messageChat{{GUID: "direct", Style: chatStyleDirect}}
			case "mention":
				msg.Text = "@Oswald follow up"
			case "group command":
				msg.Text = "/help"
			}
			g.processIncomingMessage(msg)
			if mode == "group command" {
				if calls.Load() != 0 || len(bb.paths()) != 0 || len(model.primaryRequests()) != 0 {
					t.Fatal("group command rejection did work")
				}
			} else {
				if calls.Load() != 2 || len(model.primaryRequests()) != 1 {
					t.Fatalf("calls=%d requests=%d", calls.Load(), len(model.primaryRequests()))
				}
				path := "/api/v1/chat/private-chat%3B+%3Bgroup/typing"
				if mode == "dm" {
					path = "/api/v1/chat/direct/typing"
				}
				if !bb.waitForPath(path) {
					t.Fatal("indicator did not finish")
				}
			}
		})
	}
}

func TestReplyLookupBoundsAndCancellation(t *testing.T) {
	for _, mode := range []string{"oversized", "trailing json", "timeout", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "timeout" {
					_, _ = io.Copy(io.Discard, r.Body)
					<-r.Context().Done()
					return
				}
				if mode == "oversized" {
					_, _ = fmt.Fprint(w, strings.Repeat(" ", replyLookupBodyLimit+1))
					return
				}
				_, _ = fmt.Fprint(w, `{"data":[]} {"data":[]}`)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			g := &Gateway{BlueBubblesURL: server.URL, Log: config.NewLogger(config.LevelError)}
			started := time.Now()
			result, found := g.resolveReply(ctx, threadIncoming(), true, "req-bound")
			if found || result.IsFromBot {
				t.Fatal("invalid response admitted")
			}
			if time.Since(started) > time.Second || calls.Load() > 2 {
				t.Fatal("lookup exceeded bounds")
			}
			if mode == "canceled" && calls.Load() != 0 {
				t.Fatal("canceled request submitted")
			}
		})
	}
}

func TestReplyResolverInfoMeasurement(t *testing.T) {
	log, events := captureInfoSummaries(t)
	g := &Gateway{Log: log, messageIndex: make(map[string]messageContext)}
	g.rememberBotMessage("private-attachment", "private-chat", "private-chat;+;group", "private-address", "private-body")
	msg := threadIncoming()
	msg.ReplyToGUID = "private-attachment"
	result, found := g.resolveReply(context.Background(), msg, true, "req-reply")
	if !found || !result.IsFromBot {
		t.Fatal("cached reference missing")
	}
	count := 0
	for _, event := range events() {
		if event["event"] != "gateway.reply_lookup.complete" {
			continue
		}
		count++
		if event["level"] != "info" || event["request_id"] != "req-reply" || event["record_kind"] != "measurement" || event["cache_count"] != float64(1) || event["direct_count"] != float64(1) || event["remote_count"] != float64(0) {
			t.Fatalf("measurement=%+v", event)
		}
		if _, ok := event["duration_ms"].(float64); !ok {
			t.Fatal("missing numeric duration")
		}
	}
	if count != 1 {
		t.Fatalf("measurement count=%d", count)
	}
}

func TestThreadRootAndMissingPartPredecessor(t *testing.T) {
	for _, part := range []string{"", "0:0"} {
		t.Run("part="+part, func(t *testing.T) {
			root, anchor := replyFixture("root", 1, false), replyFixture("incoming", 10, false)
			anchor.ThreadOriginatorGUID, anchor.ThreadOriginatorPart = "root", part
			server := httptest.NewServer(sqliteReplyLookup(t, []messageLookupData{root, anchor}, map[string]int64{"root": 100, "incoming": 200}))
			defer server.Close()
			g := &Gateway{BlueBubblesURL: server.URL, Log: config.NewLogger(config.LevelError)}
			msg := threadIncoming()
			msg.ThreadOriginatorPart = part
			result, found := g.resolveReply(context.Background(), msg, true, "req-root")
			if !found || !result.IsPredecessor || result.IsFromBot || result.Text != root.Text {
				t.Fatalf("root not included: %+v found=%v", result, found)
			}
		})
	}
}

func TestThreadPredecessorExactPartSQL(t *testing.T) {
	for _, name := range []string{"missing anchor part", "matching missing parts", "remote anchor part", "tied rows", "other part missing raw date", "second matching chats"} {
		t.Run(name, func(t *testing.T) {
			root, bot, other, anchor := replyFixture("root", 1, false), replyFixture("bot", 2, true), replyFixture("other", 3, true), replyFixture("incoming", 10, false)
			bot.ThreadOriginatorGUID, other.ThreadOriginatorGUID, anchor.ThreadOriginatorGUID = "root", "root", "root"
			bot.ThreadOriginatorPart, other.ThreadOriginatorPart, anchor.ThreadOriginatorPart = "0:0", "1:0", "0:0"
			dates := map[string]int64{"root": 100, "bot": 200, "other": 250, "incoming": 300}
			msg := threadIncoming()
			want := bot
			switch name {
			case "missing anchor part":
				msg.ThreadOriginatorPart, anchor.ThreadOriginatorPart = "", ""
				want = root
			case "matching missing parts":
				msg.ThreadOriginatorPart, anchor.ThreadOriginatorPart, bot.ThreadOriginatorPart = "", "", ""
			case "remote anchor part":
				msg.ThreadOriginatorPart = ""
			case "tied rows":
				dates["bot"], dates["other"], dates["incoming"] = 300, 300, 300
			case "other part missing raw date":
				dates["other"] = 0
			case "second matching chats":
				for _, m := range []*messageLookupData{&root, &bot, &anchor} {
					m.Chats = append([]messageChat{{GUID: "other-chat", Style: chatStyleGroup}}, m.Chats...)
				}
			}
			server := httptest.NewServer(sqliteReplyLookup(t, []messageLookupData{root, bot, other, anchor}, dates))
			defer server.Close()
			g := &Gateway{BlueBubblesURL: server.URL, Log: config.NewLogger(config.LevelError)}
			result, found := g.resolveReply(context.Background(), msg, true, "req-parts")
			if !found || !result.IsPredecessor || result.Text != want.Text || result.IsFromBot != want.IsFromMe || result.ChatGUID != msg.primaryChat().GUID {
				t.Fatalf("predecessor=%+v found=%v want=%s bot=%v", result, found, want.GUID, want.IsFromMe)
			}
		})
	}
}

func TestReplyResolverTerminalMeasurements(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, mode := range []string{"direct", "predecessor", "not found", "rejected", "error", "canceled"} {
			t.Run(level.String()+"/"+mode, func(t *testing.T) {
				log, events := captureInfoSummaries(t, level)
				root, anchor, bot := replyFixture("root", 1, false), replyFixture("incoming", 10, false), replyFixture("bot", 2, true)
				anchor.ThreadOriginatorGUID, bot.ThreadOriginatorGUID = "root", "root"
				anchor.ThreadOriginatorPart, bot.ThreadOriginatorPart = "0:0", "0:0"
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == "error" {
						w.WriteHeader(503)
						return
					}
					if r.Method == http.MethodGet {
						_ = json.NewEncoder(w).Encode(messageLookupResponse{})
						return
					}
					var q messageQueryRequest
					_ = json.NewDecoder(r.Body).Decode(&q)
					var rows []messageLookupData
					switch mode {
					case "direct":
						bot.GUID = "root"
						rows = []messageLookupData{bot}
					case "rejected":
						root.Chats = nil
						rows = []messageLookupData{root}
					case "predecessor":
						switch q.Where[0].Args["reply_guid"] {
						case "root":
							rows = []messageLookupData{root}
						case "incoming":
							rows = []messageLookupData{anchor}
						default:
							rows = []messageLookupData{bot}
						}
					}
					_ = json.NewEncoder(w).Encode(messageQueryResponse{Data: rows})
				}))
				defer server.Close()
				g := &Gateway{BlueBubblesURL: server.URL, Log: log}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "canceled" {
					cancel()
				}
				g.resolveReply(ctx, threadIncoming(), true, "req-terminal")
				got := events()
				if len(got) != 1 || got[0]["event"] != "gateway.reply_lookup.complete" || got[0]["level"] != "info" || got[0]["request_id"] != "req-terminal" {
					t.Fatalf("events=%+v", got)
				}
				key, status := "direct_count", "ok"
				switch mode {
				case "predecessor":
					key = "predecessor_count"
				case "not found":
					key = "not_found_count"
				case "rejected":
					key, status = "rejected_count", "rejected"
				case "error":
					key, status = "error_count", "error"
				case "canceled":
					key = "error_count"
				}
				wantCount := float64(1)
				if mode == "canceled" {
					wantCount = 0
					if got[0]["outcome"] != "canceled" {
						t.Fatal("missing cancellation outcome")
					}
				}
				if got[0][key] != wantCount || got[0]["status"] != status {
					t.Fatalf("measurement=%+v", got[0])
				}
			})
		}
	}
}

func TestReplyLookupUsesOneDeadlineAcrossGETFallback(t *testing.T) {
	var first time.Time
	calls := 0
	g := &Gateway{
		BlueBubblesURL: "http://unused.invalid", Log: config.NewLogger(config.LevelError),
		HTTPClient: &http.Client{Transport: replyRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls++
			deadline, ok := req.Context().Deadline()
			if !ok || time.Until(deadline) > replyLookupTimeout {
				t.Fatal("lookup has no bounded deadline")
			}
			if calls == 1 {
				first = deadline
			} else if !deadline.Equal(first) {
				t.Fatal("fallback renewed deadline")
			}
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("private-body")), Header: make(http.Header)}, nil
		})},
	}
	g.resolveReply(context.Background(), threadIncoming(), true, "req-deadline")
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

type replyRoundTripper func(*http.Request) (*http.Response, error)

func (f replyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestThreadReplyImageEnrichmentOnlyAfterAdmission(t *testing.T) {
	for _, botPredecessor := range []bool{false, true} {
		t.Run(fmt.Sprintf("bot=%v", botPredecessor), func(t *testing.T) {
			root, preceding, anchor := replyFixture("root", 1, false), replyFixture("preceding", 2, botPredecessor), replyFixture("incoming", 3, false)
			preceding.ThreadOriginatorGUID, anchor.ThreadOriginatorGUID = "root", "root"
			preceding.ThreadOriginatorPart, anchor.ThreadOriginatorPart = "0:0", "0:0"
			preceding.Text = ""
			preceding.Attachments = []attachment{{GUID: "private-attachment", MimeType: "image/png", TransferName: "private-filename.png"}}
			bb := newFakeBlueBubbles(t)
			defer bb.server.Close()
			bb.mu.Lock()
			bb.messageLookup = sqliteReplyLookup(t, []messageLookupData{root, preceding, anchor}, map[string]int64{"root": 100, "preceding": 200, "incoming": 300})
			bb.mu.Unlock()
			g, b, model := newIMessageTestGateway(t, bb.server.URL)
			defer b.Shutdown()
			var encoded bytes.Buffer
			if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			var downloads atomic.Int32
			g.HTTPClient = &http.Client{Transport: replyRoundTripper(func(req *http.Request) (*http.Response, error) {
				if strings.HasPrefix(req.URL.Path, "/api/v1/attachment/") {
					downloads.Add(1)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(encoded.Bytes()))}, nil
				}
				return http.DefaultTransport.RoundTrip(req)
			})}
			msg := threadIncoming()
			if !botPredecessor {
				msg.Attachments = preceding.Attachments
			}
			g.processIncomingMessage(msg)
			requests := model.primaryRequests()
			if !botPredecessor {
				if len(requests) != 0 || downloads.Load() != 0 {
					t.Fatal("ignored reply downloaded images or invoked model")
				}
				return
			}
			if len(requests) != 1 || downloads.Load() != 1 || len(requests[0].Messages[len(requests[0].Messages)-1].Images) != 1 {
				t.Fatal("reply image not enriched exactly once")
			}
			if !bb.waitForPath("/api/v1/chat/private-chat%3B+%3Bgroup/typing") {
				t.Fatal("indicator did not finish")
			}
		})
	}
}
