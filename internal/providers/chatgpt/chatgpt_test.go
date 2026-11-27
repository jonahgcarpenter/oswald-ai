package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

func TestStoreSaveLoad(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, ok, _ := store.Load(ctx); ok {
		t.Fatal("expected no token")
	}
	want := TokenSet{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct-1"}
	if err := store.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Load(ctx)
	if err != nil || !ok || got.AccessToken != want.AccessToken || got.AccountID != want.AccountID {
		t.Fatalf("load = %+v ok=%v err=%v", got, ok, err)
	}
	if Expired(got, time.Now()) {
		t.Fatal("fresh token reported expired")
	}
	expired := TokenSet{AccessToken: "x", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	if !Expired(expired, time.Now()) {
		t.Fatal("expired token not reported")
	}
}

func TestAccountIDFromIDToken(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"chatgpt_account_id": "acct-42"})
	token := "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
	if got := AccountIDFromIDToken(token); got != "acct-42" {
		t.Fatalf("account id = %q", got)
	}
	if got := AccountIDFromIDToken("bad"); got != "" {
		t.Fatalf("bad token account id = %q", got)
	}
}

func TestListModelsLive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.URL.Query().Get("client_version") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"codex-mini","context_window":272000},{"slug":"codex-full"}]}`))
	}))
	defer server.Close()
	root := t.TempDir()
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, TokenSet{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, root, "sub", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		t.Fatalf("list = err %v", err)
	}
	if len(models) != 2 || models[0].ID != "codex-mini" || models[0].Window != 272000 || models[1].ID != "codex-full" || models[1].Window != 0 {
		t.Fatalf("models = %+v", models)
	}
}

func TestListModelsSurfacesBackendError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	root := t.TempDir()
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, TokenSet{AccessToken: "synthetic-access"}); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, root, "sub", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// No static fallback: a failing live listing reports an error so the
	// subscription never advertises models that do not exist.
	if _, err := client.ListModels(ctx); err == nil {
		t.Fatal("expected error for failing live listing")
	}
}

func TestListModelsUnauthorized(t *testing.T) {
	root := t.TempDir()
	client, err := NewClient("https://chatgpt.example.com", root, "sub", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListModels(context.Background()); err == nil {
		t.Fatal("expected error for unauthorized subscription")
	}
}

// TestClientChatStreamingWithoutLogger guards the production panic where a
// subscription Chat ran with a nil gateway logger: the streaming decode path
// logs unconditionally, so a missing logger must degrade to discard output,
// never a nil-pointer panic inside the broker turn.
func TestClientChatStreamingWithoutLogger(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotBody = raw
		if body["stream"] != true || body["store"] != false {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"model\":\"codex-mini\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\"}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n"))
	}))
	defer server.Close()
	root := t.TempDir()
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, TokenSet{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, root, "sub", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var streamed []string
	resp, err := client.Chat(ctx, llm.ChatRequest{Model: "codex-mini", Messages: []llm.ChatMessage{{Role: "user", Content: "hi"}}, Stream: true}, func(chunk llm.ChatMessage) {
		streamed = append(streamed, chunk.Content)
	})
	if err != nil {
		t.Fatalf("chat = err %v", err)
	}
	if resp == nil || resp.Message.Content != "hi" || resp.DoneReason != "stop" {
		t.Fatalf("chat response = %+v", resp)
	}
	if resp.PromptTokens != 7 || resp.CompletionTokens != 3 || resp.TotalTokens != 10 {
		t.Fatalf("chat usage = %+v", resp)
	}
	if len(streamed) != 1 || streamed[0] != "hi" {
		t.Fatalf("streamed = %v", streamed)
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("request body = %s", gotBody)
	}
	if sent["model"] != "codex-mini" || sent["instructions"] != nil {
		t.Fatalf("request = %s", gotBody)
	}
	input, ok := sent["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %v", sent["input"])
	}
}

func TestClientChatMapsToolsAndHistory(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = raw
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"model\":\"codex-mini\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc-1\",\"call_id\":\"call-1\",\"name\":\"search\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()
	root := t.TempDir()
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, TokenSet{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, root, "sub", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []llm.ChatMessage{
		{Role: "system", Content: "sys-prompt"},
		{Role: "user", Content: "find x"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-0", Function: llm.ToolFunction{Name: "search", Arguments: map[string]any{"q": "x"}}}}},
		{Role: "tool", ToolCallID: "call-0", Content: "result"},
	}
	tools := []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "search", Description: "d", Parameters: llm.ToolParameters{Type: "object"}}}}
	resp, err := client.Chat(ctx, llm.ChatRequest{Model: "codex-mini", Messages: msgs, Tools: tools, ToolChoice: "required", Stream: true}, nil)
	if err != nil {
		t.Fatalf("chat = err %v", err)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].ID != "call-1" || resp.Message.ToolCalls[0].Function.Name != "search" {
		t.Fatalf("tool calls = %+v", resp.Message.ToolCalls)
	}
	if got, want := resp.Message.ToolCalls[0].Function.Arguments["q"], "x"; got != want {
		t.Fatalf("tool args = %+v", resp.Message.ToolCalls[0].Function.Arguments)
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("request body = %s", gotBody)
	}
	if sent["instructions"] != "sys-prompt" {
		t.Fatalf("instructions = %v", sent["instructions"])
	}
	if sent["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v", sent["tool_choice"])
	}
	input, _ := sent["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input items = %v", sent["input"])
	}
	toolsSent, _ := sent["tools"].([]any)
	if len(toolsSent) != 1 {
		t.Fatalf("tools = %v", sent["tools"])
	}
}

func TestClientChatUnauthorized(t *testing.T) {
	root := t.TempDir()
	client, err := NewClient("https://chatgpt.example.com", root, "sub", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Chat(context.Background(), llm.ChatRequest{Model: "codex-mini", Messages: []llm.ChatMessage{{Role: "user", Content: "hi"}}, Stream: true}, nil); err == nil {
		t.Fatal("expected error for unauthorized subscription")
	}
}

// TestClientChatHistoryPartTypes guards the Codex wire rule that assistant
// history replays as output_text parts: sending input_text there fails the
// whole turn with a 400, which only surfaces on the second turn of a
// conversation when history first appears.
func TestClientChatHistoryPartTypes(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = raw
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"model\":\"codex-mini\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"m-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{}}}\n\n"))
	}))
	defer server.Close()
	root := t.TempDir()
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, TokenSet{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, root, "sub", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []llm.ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "again"},
	}
	if _, err := client.Chat(ctx, llm.ChatRequest{Model: "codex-mini", Messages: msgs, Stream: true}, nil); err != nil {
		t.Fatalf("chat = err %v", err)
	}
	var sent struct {
		Input []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("request body = %s", gotBody)
	}
	if len(sent.Input) != 3 {
		t.Fatalf("input items = %s", gotBody)
	}
	want := map[string]string{"user": "input_text", "assistant": "output_text"}
	for i, item := range sent.Input {
		if len(item.Content) != 1 {
			t.Fatalf("item %d content = %v", i, item.Content)
		}
		if got, ok := want[item.Role]; !ok || item.Content[0].Type != got {
			t.Fatalf("item %d role %q part type = %q", i, item.Role, item.Content[0].Type)
		}
	}
}
func TestAuthorizeURLHasPKCE(t *testing.T) {
	url := BuildAuthorizeURL("http://localhost:1455/auth/callback", "state-1", "challenge-1")
	if !strings.Contains(url, "code_challenge=challenge-1") || !strings.Contains(url, "app_EMoamEEZ73f0CkXaXp7hrann") {
		t.Fatalf("authorize url = %q", url)
	}
}

func TestPendingRoundTrip(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, ok, _ := store.LoadPending(ctx); ok {
		t.Fatal("expected no pending handoff")
	}
	want := PendingAuth{Verifier: "synthetic-verifier", State: "synthetic-state", RedirectURI: "http://localhost:1455/auth/callback", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err := store.SavePending(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.LoadPending(ctx)
	if err != nil || !ok || got.Verifier != want.Verifier || got.State != want.State {
		t.Fatalf("pending = %+v ok=%v err=%v", got, ok, err)
	}
	if PendingExpired(got, time.Now()) {
		t.Fatal("fresh pending reported expired")
	}
	if !PendingExpired(PendingAuth{ExpiresAt: time.Now().Add(-time.Hour).Unix()}, time.Now()) {
		t.Fatal("expired pending not reported")
	}
	if err := store.ClearPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.LoadPending(ctx); ok {
		t.Fatal("pending not cleared")
	}
}

func TestExtractCode(t *testing.T) {
	if got, err := ExtractCode("synthetic-code", "s"); err != nil || got != "synthetic-code" {
		t.Fatalf("bare code = %q err=%v", got, err)
	}
	if got, err := ExtractCode("http://localhost:1455/auth/callback?code=synthetic-code&state=s", "s"); err != nil || got != "synthetic-code" {
		t.Fatalf("redirect url = %q err=%v", got, err)
	}
	if _, err := ExtractCode("http://localhost:1455/auth/callback?code=x&state=other", "s"); err == nil {
		t.Fatal("expected state mismatch")
	}
	if _, err := ExtractCode("http://localhost:1455/auth/callback?error=access_denied&state=s", "s"); err == nil {
		t.Fatal("expected denial error")
	}
	if _, err := ExtractCode("", "s"); err == nil {
		t.Fatal("expected empty error")
	}
}

func TestStartFinishHandoff(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"chatgpt_account_id": "acct-7"})
	idToken := "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"id_token":"` + idToken + `"}`))
	}))
	defer server.Close()
	old := authBaseURL
	authBaseURL = server.URL
	defer func() { authBaseURL = old }()

	root := t.TempDir()
	ctx := context.Background()
	authURL, _, err := Start(ctx, root, "sub", time.Hour)
	if err != nil || !strings.Contains(authURL, "oauth/authorize") {
		t.Fatalf("start = %q err=%v", authURL, err)
	}
	if err := Finish(ctx, root, "sub", "synthetic-code", nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	tokens, ok, err := store.Load(ctx)
	if err != nil || !ok || tokens.AccessToken != "synthetic-access" || tokens.AccountID != "acct-7" {
		t.Fatalf("tokens = %+v ok=%v err=%v", tokens, ok, err)
	}
	if _, ok, _ := store.LoadPending(ctx); ok {
		t.Fatal("pending not cleared after finish")
	}
}

func TestFinishWithoutStart(t *testing.T) {
	if err := Finish(context.Background(), t.TempDir(), "sub", "synthetic-code", nil); err == nil {
		t.Fatal("expected error without pending handoff")
	}
}
