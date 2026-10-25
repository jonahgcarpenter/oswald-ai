package clarify

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
)

func testContext(gateway string) context.Context {
	principal := identity.Principal{CanonicalUserID: "alice", Gateway: gateway, ExternalID: "alice"}
	switch gateway {
	case "discord":
		principal.ExternalID = "123"
		principal.Assurance = identity.AssuranceDiscordGateway
	case "imessage":
		principal.ExternalID = "+15551234567"
		principal.Assurance = identity.AssuranceBlueBubblesWebhook
	case "openai":
		principal.ExternalID = identity.LocalOpenAIIdentifier
		principal.Assurance = identity.AssuranceLocalLoopback
	case "cron":
		principal.ExternalID = "cron:alice"
		principal.Assurance = identity.AssuranceCronScheduler
	}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req-1", SessionID: "discord:dm:alice", Workload: "foreground"})
	return WithNotifier(ctx, func(Notice) error { return nil })
}

func testContextWithNotifier(gateway string, notify Notifier) context.Context {
	principal := identity.Principal{CanonicalUserID: "alice", Gateway: gateway, ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req-1", SessionID: "discord:dm:alice", Workload: "foreground"})
	return WithNotifier(ctx, notify)
}

func TestDefinitionMatchesAdvertisedSchema(t *testing.T) {
	def := Definition()
	if def.Name != Name || !strings.Contains(def.Description, "multi_select") {
		t.Fatalf("definition = %+v", def)
	}
	questions := def.Parameters.Properties["questions"]
	if questions.Type != "array" || questions.MinItems == nil || *questions.MinItems != 1 || questions.MaxItems == nil || *questions.MaxItems != 5 || questions.Items == nil {
		t.Fatalf("questions schema = %+v", questions)
	}
	item := questions.Items
	if len(item.Required) != 1 || item.Required[0] != "question" {
		t.Fatalf("question item schema = %+v", item)
	}
	choices := item.Properties["choices"]
	if choices.Type != "array" || choices.MaxItems == nil || *choices.MaxItems != 4 {
		t.Fatalf("choices schema = %+v", choices)
	}
	if len(def.Parameters.Required) != 1 || def.Parameters.Required[0] != "questions" {
		t.Fatalf("required = %+v", def.Parameters.Required)
	}
}

func TestPendingSingleFlightAndFulfill(t *testing.T) {
	Reset()
	if !Request(Pending{Profile: "alice", SessionKey: "s1", Answer: make(chan Answer, 1)}) {
		t.Fatal("first request rejected")
	}
	if Request(Pending{Profile: "alice", SessionKey: "s1", Answer: make(chan Answer, 1)}) {
		t.Fatal("second request admitted while one is pending")
	}
	if !Has("alice", "s1") {
		t.Fatal("pending ask not reported")
	}
	if Fulfill("alice", "other", "1: a") {
		t.Fatal("foreign session fulfilled the ask")
	}
	if !Fulfill("alice", "s1", "1: a") {
		t.Fatal("answer did not fulfill the ask")
	}
	// The ask stays parked across sequential questions: further replies
	// keep fulfilling until the handler withdraws it.
	if !Has("alice", "s1") {
		t.Fatal("fulfilled ask dropped while the form is unfinished")
	}
	if !Fulfill("alice", "s1", "1: b") {
		t.Fatal("second answer did not fulfill the parked ask")
	}
	if _, ok := Take("alice", "s1"); !ok {
		t.Fatal("completed ask not withdrawable")
	}
	if Has("alice", "s1") {
		t.Fatal("withdrawn ask still pending")
	}
	if Fulfill("alice", "s1", "1: a") {
		t.Fatal("withdrawn ask fulfilled twice")
	}
}

func TestExpiredAskNeverSwallowsMessage(t *testing.T) {
	Reset()
	old := ClarifyWindow
	ClarifyWindow = -time.Second
	defer func() { ClarifyWindow = old }()
	if !Request(Pending{Profile: "alice", SessionKey: "s1", Answer: make(chan Answer, 1)}) {
		t.Fatal("request rejected")
	}
	if Fulfill("alice", "s1", "1: a") {
		t.Fatal("expired ask swallowed a normal message")
	}
	if Has("alice", "s1") {
		t.Fatal("expired ask still reported")
	}
}

func TestDenySessionUnblocksWaiter(t *testing.T) {
	Reset()
	answer := make(chan Answer, 1)
	if !Request(Pending{Profile: "alice", SessionKey: "s1", Answer: answer}) {
		t.Fatal("request rejected")
	}
	if !DenySession("alice", "s1") {
		t.Fatal("deny found nothing")
	}
	if _, ok := <-answer; ok {
		t.Fatal("waiter not signaled as cancelled")
	}
	if DenySession("alice", "s1") {
		t.Fatal("second deny found an ask")
	}
}

func TestParseAnswerWholeReply(t *testing.T) {
	single := Question{Question: "Color?"}
	response := ParseAnswer(single, "blue")
	if response.Status != statusAnswered || response.UserResponse != "blue" || response.Question != "Color?" {
		t.Fatalf("single open response = %+v", response)
	}
	// A stale "N:" prefix is tolerated and stripped.
	response = ParseAnswer(single, "2: blue")
	if response.Status != statusAnswered || response.UserResponse != "blue" {
		t.Fatalf("prefixed response = %+v", response)
	}
	multi := Question{Question: "Size?", MultiSelect: true}
	response = ParseAnswer(multi, "s, m")
	picks, ok := response.UserResponse.([]string)
	if !ok || len(picks) != 2 {
		t.Fatalf("multi response = %+v", response)
	}
	response = ParseAnswer(multi, "skip")
	if response.Status != statusSkipped || response.UserResponse != nil {
		t.Fatalf("skip not honored: %+v", response)
	}
}

func TestRenderAskOneLettersAndProgress(t *testing.T) {
	text := RenderAskOne(Question{Question: "Color?", Choices: []string{"red", "blue"}}, 5)
	if strings.Contains(text, "1. Color?") || !strings.Contains(text, "A. red (Recommended)") || !strings.Contains(text, "B. blue") || !strings.Contains(text, "C. Other") || !strings.Contains(text, "Reply with the letter, your own answer, or skip.") || !strings.Contains(text, "5 minutes") {
		t.Fatalf("single ask text = %q", text)
	}
	// No transport-specific hint lines: the footer alone describes the format.
	if strings.Contains(text, "Reply with the letter of your pick") {
		t.Fatalf("redundant hint line in ask text = %q", text)
	}
	// Model-supplied prefixes ride through untouched: the renderer adds none.
	prefixed := RenderAskOne(Question{Question: "Q3 - Size?", Choices: []string{"s", "m"}}, 1)
	if !strings.Contains(prefixed, "Q3 - Size?") || strings.Contains(prefixed, "2. Size?") || strings.Contains(prefixed, "Question 2 of 3") || !strings.Contains(prefixed, "about a minute left") {
		t.Fatalf("prefixed ask text = %q", prefixed)
	}
	open := RenderAskOne(Question{Question: "Why?"}, 5)
	if !strings.Contains(open, "Reply with your answer or skip.") || strings.Contains(open, "letter") {
		t.Fatalf("open ask text = %q", open)
	}
	multi := RenderAskOne(Question{Question: "Toppings?", Choices: []string{"cheese", "ham"}, MultiSelect: true}, 5)
	if !strings.Contains(multi, "Reply with the letter(s) in a comma-separated list, your own answer, or skip.") || strings.Contains(multi, "pick several letters") || !strings.Contains(multi, "C. Other") {
		t.Fatalf("multi ask text = %q", multi)
	}
}

func TestParseAnswersResolveLetters(t *testing.T) {
	lettered := Question{Question: "Color?", Choices: []string{"red", "blue"}}
	for text, want := range map[string]string{"B": "blue", "b": "blue", " b ": "blue", "A": "red", "a": "red"} {
		response := ParseAnswer(lettered, text)
		if response.UserResponse != want {
			t.Fatalf("letter %q = %+v, want %q", text, response, want)
		}
	}
	response := ParseAnswer(lettered, "Z")
	if response.UserResponse != "Z" {
		t.Fatalf("out-of-range letter = %+v", response)
	}
	multi := Question{Question: "Toppings?", Choices: []string{"cheese", "ham", "mushroom"}, MultiSelect: true}
	response = ParseAnswer(multi, "A, c")
	picks, ok := response.UserResponse.([]string)
	if !ok || len(picks) != 2 || picks[0] != "cheese" || picks[1] != "mushroom" {
		t.Fatalf("multi letters = %+v", response)
	}
	response = ParseAnswer(multi, "anchovies")
	if picks, ok := response.UserResponse.([]string); !ok || len(picks) != 1 || picks[0] != "anchovies" {
		t.Fatalf("free-text multi = %+v", response)
	}
}

func TestHandlerBlocksUntilAnswer(t *testing.T) {
	Reset()
	ctx := testContext("discord")
	done := make(chan string, 1)
	go func() {
		result, err := NewHandler()(ctx, map[string]interface{}{
			"questions": []interface{}{map[string]interface{}{"question": "Color?", "choices": []interface{}{"red", "blue"}}},
		})
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- result.Content
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !Has("alice", "discord:dm:alice") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !Fulfill("alice", "discord:dm:alice", "B") {
		t.Fatal("answer did not fulfill the parked ask")
	}
	content := <-done
	var payload struct {
		Responses []Response `json:"responses"`
		Outcome   string     `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil || payload.Outcome != outcomeSubmitted || len(payload.Responses) != 1 || payload.Responses[0].UserResponse != "blue" {
		t.Fatalf("tool result = %q, %v", content, err)
	}
}

func TestHandlerAsksSequentially(t *testing.T) {
	Reset()
	var mu sync.Mutex
	var notices []Notice
	ctx := testContextWithNotifier("discord", func(notice Notice) error {
		mu.Lock()
		defer mu.Unlock()
		notices = append(notices, notice)
		return nil
	})
	done := make(chan string, 1)
	go func() {
		result, err := NewHandler()(ctx, map[string]interface{}{
			"questions": []interface{}{
				map[string]interface{}{"question": "Color?", "choices": []interface{}{"red", "blue"}},
				map[string]interface{}{"question": "Size?", "choices": []interface{}{"s", "m"}},
			},
		})
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- result.Content
	}()
	waitParked := func() {
		deadline := time.Now().Add(2 * time.Second)
		for !Has("alice", "discord:dm:alice") && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitParked()
	if !Fulfill("alice", "discord:dm:alice", "B") {
		t.Fatal("first answer did not fulfill the parked ask")
	}
	waitParked()
	if !Fulfill("alice", "discord:dm:alice", "a") {
		t.Fatal("second answer did not fulfill the parked ask")
	}
	content := <-done
	var payload struct {
		Responses []Response `json:"responses"`
		Outcome   string     `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil || payload.Outcome != outcomeSubmitted || len(payload.Responses) != 2 || payload.Responses[0].UserResponse != "blue" || payload.Responses[1].UserResponse != "s" {
		t.Fatalf("tool result = %q, %v", content, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notices) != 2 || notices[0].Question.Question != "Color?" || notices[1].Question.Question != "Size?" {
		t.Fatalf("asks = %+v", notices)
	}
}

func TestHandlerTimeoutKeepsPartialAnswers(t *testing.T) {
	Reset()
	old := ClarifyWindow
	ClarifyWindow = 300 * time.Millisecond
	defer func() { ClarifyWindow = old }()
	ctx := testContext("discord")
	done := make(chan string, 1)
	go func() {
		result, err := NewHandler()(ctx, map[string]interface{}{
			"questions": []interface{}{
				map[string]interface{}{"question": "Color?", "choices": []interface{}{"red", "blue"}},
				map[string]interface{}{"question": "Size?"},
			},
		})
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- result.Content
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !Has("alice", "discord:dm:alice") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !Fulfill("alice", "discord:dm:alice", "A") {
		t.Fatal("first answer did not fulfill the parked ask")
	}
	content := <-done
	var payload struct {
		Responses []Response `json:"responses"`
		Outcome   string     `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil || payload.Outcome != outcomeTimedOut || len(payload.Responses) != 2 {
		t.Fatalf("tool result = %q, %v", content, err)
	}
	if payload.Responses[0].UserResponse != "red" || payload.Responses[1].Status != statusUnanswered {
		t.Fatalf("partial answers = %+v", payload.Responses)
	}
}

func TestHandlerRejectsBounds(t *testing.T) {
	Reset()
	ctx := testContext("discord")
	for _, args := range []map[string]interface{}{
		{},
		{"questions": []interface{}{}},
		{"questions": []interface{}{map[string]interface{}{"question": "  "}}},
		{"bogus": true, "questions": []interface{}{map[string]interface{}{"question": "ok?"}}},
	} {
		if _, err := NewHandler()(ctx, args); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
}

func TestHandlerFailsFastOffInteractive(t *testing.T) {
	Reset()
	for _, gateway := range []string{"openai", "cron"} {
		ctx := testContext(gateway)
		if _, err := NewHandler()(ctx, map[string]interface{}{
			"questions": []interface{}{map[string]interface{}{"question": "ok?"}},
		}); err == nil {
			t.Fatalf("parked on unattended transport %q", gateway)
		}
	}
	if Has("alice", "discord:dm:alice") {
		t.Fatal("failed handler left a parked ask")
	}
}

func TestHandlerUndeliveredWhenAskFails(t *testing.T) {
	Reset()
	principal := identity.Principal{CanonicalUserID: "alice", Gateway: "imessage", ExternalID: "+15551234567", Assurance: identity.AssuranceBlueBubblesWebhook}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req-1", SessionID: "imessage:dm:alice"})
	ctx = WithNotifier(ctx, func(Notice) error { return context.DeadlineExceeded })
	result, err := NewHandler()(ctx, map[string]interface{}{
		"questions": []interface{}{map[string]interface{}{"question": "ok?"}},
	})
	if err != nil {
		t.Fatalf("undelivered err = %v", err)
	}
	if !strings.Contains(result.Content, outcomeUndelivered) {
		t.Fatalf("undelivered content = %q", result.Content)
	}
	if Has("alice", "imessage:dm:alice") {
		t.Fatal("undelivered ask still pending")
	}
}

func TestHandlerTimesOut(t *testing.T) {
	Reset()
	old := ClarifyWindow
	ClarifyWindow = 20 * time.Millisecond
	defer func() { ClarifyWindow = old }()
	ctx := testContext("discord")
	result, err := NewHandler()(ctx, map[string]interface{}{
		"questions": []interface{}{map[string]interface{}{"question": "ok?"}},
	})
	if err != nil {
		t.Fatalf("timeout err = %v", err)
	}
	if !strings.Contains(result.Content, outcomeTimedOut) {
		t.Fatalf("timeout content = %q", result.Content)
	}
}
