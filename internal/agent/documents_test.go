package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/gateway/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestDocumentAdmissionSurvivesModelFailureAndDoesNotPolluteEvidence(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/fail=%t", streaming, failed), func(t *testing.T) {
				chat := &fakeChatter{responses: []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "Processing your document."}}}}
				if failed {
					chat.outcomes = []fakeChatOutcome{{err: errors.New("provider failed")}}
				}
				a, store := newTestAgent(t, chat, nil, nil)
				var logs bytes.Buffer
				a.log = config.NewLogger(config.LevelInfo)
				a.log.SetOutput(&logs)
				called := 0
				ctx := requestctx.WithMetadata(context.Background(), requestctx.Metadata{PublicUserText: "authored", GroupGateway: "discord", GroupChatID: "room", DocumentLoader: &requestctx.DocumentLoader{FileCount: 1, SourceBytes: 20 << 20, Load: func(ctx context.Context) ([]requestctx.DocumentUpload, error) {
					called++
					p, _ := requestctx.PrincipalFromContext(ctx)
					if p.CanonicalUserID != "user-1" {
						t.Fatal("loader did not receive refreshed actor")
					}
					if requestctx.MetadataFromContext(ctx).CurrentUserText != "authored" {
						t.Fatal("authored evidence changed")
					}
					return []requestctx.DocumentUpload{{Filename: "private-document-canary.txt", MediaType: "text/plain", AdmissionKey: "upload", Data: []byte("private-source-canary")}}, nil
				}}})
				req := Request{RequestID: "doc", Principal: identity.Principal{CanonicalUserID: "user-1", ExternalID: "user-1", Gateway: "discord", Assurance: identity.AssuranceDiscordGateway}, SessionKey: "session", Prompt: "authored"}
				if streaming {
					req.StreamFunc = func(StreamChunk) {}
				}
				response, err := a.Process(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				docs, err := store.ListUserDocuments(context.Background(), memory.DocumentScope{UserID: "user-1"})
				if err != nil || len(docs) != 1 || called != 1 {
					t.Fatalf("docs=%v calls=%d err=%v", docs, called, err)
				}
				if len(chat.requests) != 1 || !messagesContain(chat.requests[0].Messages, "private-document-canary") || messagesContain(chat.requests[0].Messages, "private-source-canary") {
					t.Fatal("queued catalog missing or source prematurely exposed")
				}
				if !failed {
					turns, err := store.RecentSessionTurns("user-1", "session", response.SessionGeneration, 10)
					if err != nil || len(turns) != 1 || turns[0].UserText != "authored" {
						t.Fatalf("authored text polluted: %v %v", turns, err)
					}
				}
				if strings.Contains(logs.String(), "private-document-canary") || strings.Contains(logs.String(), "private-source-canary") {
					t.Fatal("document payload leaked to logs")
				}
				if strings.Count(logs.String(), `"event":"agent.documents.loaded"`) != 1 {
					t.Fatal("missing document measurement")
				}
			})
		}
	}
}

func TestDocumentContextFitsActualRemainingBudget(t *testing.T) {
	base := []llm.ChatMessage{{Role: "system", Content: "policy"}, {Role: "user", Content: "authored"}}
	document := llm.ChatMessage{Role: "user", Content: "Untrusted document reference\n{\"text\":\"" + strings.Repeat("large ", 500) + "\"}\n{\"id\":\"small\"}\n"}
	limit := budget.EstimateRequest(base, nil)
	got := fitDocumentContext(base, document, nil, limit)
	if len(got) != len(base) || budget.EstimateRequest(got, nil) > limit {
		t.Fatal("optional documents overflowed fitting required context")
	}
	small := llm.ChatMessage{Role: "user", Content: "Untrusted document reference\n{\"id\":\"small\"}\n"}
	limit = budget.EstimateRequest(append(append([]llm.ChatMessage(nil), base...), small), nil)
	got = fitDocumentContext(base, document, nil, limit)
	if len(got) != 3 || got[2].Content != small.Content || budget.EstimateRequest(got, nil) > limit {
		t.Fatal("did not skip large record and fit whole smaller record")
	}
	if base[1].Content != "authored" {
		t.Fatal("mutated authored prompt")
	}
}

func TestDocumentContextRefitsAfterActiveCompaction(t *testing.T) {
	compactor := &fakeForegroundCompactor{artifact: memory.SummaryArtifact{Narrative: "checkpoint"}}
	doc := llm.ChatMessage{Role: "user", Content: "Untrusted reference\n{\"text\":\"" + strings.Repeat("large ", 500) + "\"}\n"}
	state := newForegroundCompactionState(compactor, 10000, "policy", "", "authored", nil, nil, []memory.SessionTurn{{ID: 1, UserText: "old", AssistantText: "answer"}}, nil)
	required := []llm.ChatMessage{{Role: "system", Content: "policy"}, {Role: "user", Content: memory.RenderTransientSessionSummary(compactor.artifact)}, {Role: "user", Content: "authored"}}
	state.inputLimit = budget.EstimateRequest(required, nil)
	state.documentContext = &doc
	rebuilt, stats, err := state.prepare(context.Background(), append(required, doc), nil, true)
	if err != nil || !stats.Compacted || len(rebuilt) != len(required) || stats.EstimatedAfter > state.inputLimit {
		t.Fatalf("compaction reinserted over-budget documents: %+v %v", stats, err)
	}
	// Retain the source records for a later checkpoint with sufficient capacity.
	if state.documentContext.Content != doc.Content {
		t.Fatal("lost unselected source records")
	}
}

func TestAgentSmallContextOmitsDocumentsWithoutDroppingAuthoredText(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Message: llm.ChatMessage{Role: "assistant", Content: "answer"}}}}
	a, store := newTestAgent(t, chat, nil, nil)
	a.budget = budget.ContextBudget{PromptLimit: 128}
	if _, err := store.AcceptUserDocuments(context.Background(), "user-1", []memory.DocumentUpload{{Filename: "catalog-canary.txt", MediaType: "text/plain", Data: []byte("source")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := processAgent(a, "small", "homeassistant", "session", "user-1", "User", "authored", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(chat.requests) != 1 {
		t.Fatal("unexpected model calls")
	}
	req := chat.requests[0]
	if budget.EstimateRequest(req.Messages, req.Tools) > 128 || !messagesContain(req.Messages, "authored") || messagesContain(req.Messages, "catalog-canary") {
		t.Fatalf("documents exceeded required context budget: estimate=%d messages=%+v", budget.EstimateRequest(req.Messages, req.Tools), req.Messages)
	}
}

func TestDocumentQuotaRejectedBeforeGET(t *testing.T) {
	a, store := newTestAgent(t, &fakeChatter{}, nil, nil)
	ctx := context.Background()
	fillDocumentLibrary(t, store.Store)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	loader := routing.NewDocumentLoader(server.Client(), []routing.DocumentDownload{{Filename: "new.txt", URL: server.URL, Size: 6}}, func(*url.URL) bool { return true })
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{DocumentLoader: loader})
	if _, err := a.loadDocumentContext(ctx, "user-1", "", a.log); err == nil {
		t.Fatal("full library admitted upload")
	}
	if calls.Load() != 0 {
		t.Fatal("quota rejection happened after GET")
	}
}

func TestDocumentReservationReleasedAfterCancellationOrLoadError(t *testing.T) {
	for _, cancelDownload := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelDownload), func(t *testing.T) {
			a, store := newTestAgent(t, &fakeChatter{}, nil, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			loader := &requestctx.DocumentLoader{FileCount: 1, SourceBytes: 20 << 20, Load: func(context.Context) ([]requestctx.DocumentUpload, error) {
				if cancelDownload {
					cancel()
					return nil, context.Canceled
				}
				return nil, fmt.Errorf("download failed")
			}}
			ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{DocumentLoader: loader})
			_, err := a.loadDocumentContext(ctx, "user-1", "", a.log)
			if err == nil || (cancelDownload && !errors.Is(err, context.Canceled)) {
				t.Fatalf("load error=%v", err)
			}
			// Filling all 50 document slots proves no live upload reservation remains.
			fillDocumentLibrary(t, store.Store)
		})
	}
}

func fillDocumentLibrary(t *testing.T, store *memory.Store) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if _, err := store.AcceptUserDocuments(ctx, "user-1", []memory.DocumentUpload{{Filename: fmt.Sprintf("%d.txt", i), MediaType: "text/plain", Data: []byte("source")}}); err != nil {
			t.Fatal(err)
		}
		job, err := store.ClaimUserDocumentExtraction(ctx, "test")
		if err != nil || job == nil {
			t.Fatalf("claim=%v %v", job, err)
		}
		if err = store.CompleteUserDocumentExtraction(ctx, job, []memory.DocumentChunk{{Ordinal: 0, Text: "source", Method: "text"}}, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDocumentContextPrivateRetrievalAndCompaction(t *testing.T) {
	a, store := newTestAgent(t, &fakeChatter{}, nil, nil)
	ctx := context.Background()
	docs, err := store.AcceptUserDocuments(ctx, "user-1", []memory.DocumentUpload{{Filename: "reference.txt", MediaType: "text/plain", Data: []byte("source")}})
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimUserDocumentExtraction(ctx, "test")
	if err != nil || job == nil {
		t.Fatalf("job=%v err=%v", job, err)
	}
	if err = store.CompleteUserDocumentExtraction(ctx, job, []memory.DocumentChunk{{Ordinal: 0, Text: strings.Repeat("text ", 2000) + "private-reference-canary " + strings.Repeat("%", 250), Locator: "page 1", Method: "text"}}, false); err != nil {
		t.Fatal(err)
	}
	message, err := a.loadDocumentContext(ctx, "user-1", "private-reference-canary", a.log)
	if err != nil {
		t.Fatal(err)
	}
	if message.Role != "user" || !strings.Contains(message.Content, docs[0].ID) || !strings.Contains(message.Content, "private-reference-canary") || len(message.Content) > 12000 {
		t.Fatal("invalid bounded context")
	}
	symbols, err := a.loadDocumentContext(ctx, "user-1", strings.Repeat("%", 251), a.log)
	if err != nil || !strings.Contains(symbols.Content, strings.Repeat("%", 250)) {
		t.Fatalf("query prefix gained ellipsis or late match was lost: %v", err)
	}
	other, err := a.loadDocumentContext(ctx, "user-2", "private-reference-canary", a.log)
	if err != nil || other.Content != "" {
		t.Fatalf("other tenant context=%v err=%v", other, err)
	}
	compactor := &fakeForegroundCompactor{artifact: memory.SummaryArtifact{Narrative: "checkpoint"}}
	state := newForegroundCompactionState(compactor, 30000, "policy", "", "authored", nil, nil, []memory.SessionTurn{{ID: 1, UserText: "old", AssistantText: "answer"}}, nil)
	state.documentContext = &message
	rebuilt, stats, err := state.prepare(ctx, []llm.ChatMessage{message}, nil, true)
	if err != nil || !stats.Compacted || !messagesContain(rebuilt, "private-reference-canary") {
		t.Fatalf("compaction lost document context: %v", err)
	}
	if compactor.calls[0].turns[0].UserText != "old" {
		t.Fatal("document reference entered summary source")
	}
}
