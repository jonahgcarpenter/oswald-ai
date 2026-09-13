package imessage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
	"github.com/jonahgcarpenter/oswald-ai/internal/soul"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestIMessageStreamsModelToolRoundsButDeliversOnlyFinalResponse(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	var deliveryMu sync.Mutex
	var deliveries []string
	blueBubbles := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/message/text" || r.URL.Path == "/api/v1/message/attachment" {
			deliveryMu.Lock()
			deliveries = append(deliveries, r.URL.Path)
			deliveryMu.Unlock()
		}
		if r.URL.Path != "/api/v1/message/attachment" {
			bb.server.Config.Handler.ServeHTTP(w, r)
			return
		}
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Error(err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, header, err := r.FormFile("attachment")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil || header.Filename != "report.txt" || string(data) != "final attachment" {
			t.Errorf("attachment=%q data=%q err=%v", header.Filename, data, err)
		}
		_, _ = io.WriteString(w, `{"data":{"guid":"attachment-1"}}`)
	}))
	defer blueBubbles.Close()

	stages := make(chan int, 2)
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	stop := make(chan struct{})
	var requests, executions atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round := int(requests.Add(1))
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || round > 2 {
			t.Errorf("unexpected model request %d: %s %s", round, r.Method, r.URL.Path)
			http.Error(w, "synchronous chat only", http.StatusBadRequest)
			return
		}
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if !body.Stream {
			t.Error("foreground model request did not set stream=true")
		}
		if round == 2 {
			found := false
			for _, message := range body.Messages {
				found = found || (message.Role == "tool" && message.ToolCallID == "call-report" && message.Content == "report ready")
			}
			if !found || executions.Load() != 1 {
				t.Error("second round did not contain the executed tool's correlated result")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		content := "I will prepare the report."
		if round == 2 {
			content = "Final reply"
		}
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"private reasoning\",\"content\":%q}}]}\n\n", content)
		w.(http.Flusher).Flush()
		stages <- round
		select {
		case <-release[round-1]:
		case <-stop:
			return
		case <-r.Context().Done():
			return
		}
		if round == 1 {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-report\",\"function\":{\"name\":\"test.report\",\"arguments\":\"{}\"}}]}}]}\n\n")
		} else {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{\"content\":\" only.\"}}]}\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer model.Close()

	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oswald.db")
	memories := memorytest.NewStore(t, dbPath, log)
	links := accounts.NewService(dbPath, memories, nil, log)
	defer links.Close()
	soulPath := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(soulPath, []byte("You are Oswald."), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# test.report\n\n## Description\n\nPrepare a report.\n\n## Parameters\n\n| Name | Type | Required | Description |\n| --- | --- | --- | --- |\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.NewFromDirectory(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterHandler("test.report", governance.ToolPolicy{}, func(context.Context, map[string]interface{}) (governance.Result, error) {
		executions.Add(1)
		return governance.Result{Content: "report ready", Outcome: governance.OutcomeProductive, Attachments: []media.OutputAttachment{{Filename: "report.txt", MIMEType: "text/plain", Data: []byte("final attachment")}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	ai := agent.NewAgent(llm.NewGatewayClient(model.URL, "", "", log), reg, "test-model", soul.NewStore(soulPath), memories, budget.ContextBudget{PromptLimit: 100000}, governance.GlobalPolicy{MaxExecutions: 12, MaxToolIterations: 8}, log)
	b := broker.NewBroker(ai, 1, log)
	commandService, err := commands.NewServiceWithCommands()
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{BlueBubblesURL: blueBubbles.URL, BlueBubblesPassword: "pw", Links: links, Runtime: gatewayruntime.Dependencies{Commands: commandService, Log: log}, Log: log, Broker: b, messageIndex: make(map[string]messageContext), contactNames: make(map[string]contactNameCacheEntry)}
	b.Start()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.processIncomingMessage(webhookMessage{GUID: "stream-inbound", Text: "Prepare a report", Handle: messageHandle{Address: "+15551234567"}, Chats: []messageChat{{GUID: "chat-direct", Style: chatStyleDirect}}})
	}()
	defer func() {
		close(stop)
		b.Shutdown()
		<-done
	}()
	for round := 1; round <= 2; round++ {
		select {
		case got := <-stages:
			if got != round {
				t.Fatalf("round=%d want %d", got, round)
			}
		case <-done:
			t.Fatal("inbound processing ended before streaming rounds completed")
		case <-time.After(5 * time.Second):
			t.Fatal("model stream did not reach expected round")
		}
		deliveryMu.Lock()
		premature := append([]string(nil), deliveries...)
		deliveryMu.Unlock()
		if len(premature) != 0 {
			t.Fatalf("intermediate model content or attachments delivered: %v", premature)
		}
		close(release[round-1])
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("final delivery did not complete")
	}
	if requests.Load() != 2 || executions.Load() != 1 {
		t.Fatalf("model requests=%d tool executions=%d", requests.Load(), executions.Load())
	}
	if sent := bb.sentMessages(); len(sent) != 1 || sent[0].Message != "Final reply only." {
		t.Fatalf("expected only final text, got %+v", sent)
	}
	deliveryMu.Lock()
	defer deliveryMu.Unlock()
	if want := []string{"/api/v1/message/attachment", "/api/v1/message/text"}; !reflect.DeepEqual(deliveries, want) {
		t.Fatalf("deliveries=%v want %v", deliveries, want)
	}
}
