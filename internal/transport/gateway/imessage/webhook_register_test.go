package imessage

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

type webhookRegisterFixture struct {
	server    *httptest.Server
	mu        sync.Mutex
	listBody  string
	listCode  int
	posts     []webhookRegisterRequest
	postCode  int
	postCount int
	getCount  int
}

func newWebhookRegisterFixture(t *testing.T, listBody string, listCode, postCode int) *webhookRegisterFixture {
	t.Helper()
	fx := &webhookRegisterFixture{listBody: listBody, listCode: listCode, postCode: postCode}
	fx.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fx.mu.Lock()
		defer fx.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/webhook" && r.Method == http.MethodGet:
			fx.getCount++
			w.WriteHeader(fx.listCode)
			_, _ = io.WriteString(w, fx.listBody)
		case r.URL.Path == "/api/v1/webhook" && r.Method == http.MethodPost:
			fx.postCount++
			var req webhookRegisterRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode register request: %v", err)
			}
			fx.posts = append(fx.posts, req)
			w.WriteHeader(fx.postCode)
			_, _ = io.WriteString(w, `{"status":200,"message":"Success","data":{"id":48}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fx.server.Close)
	return fx
}

func (fx *webhookRegisterFixture) gateway() *Gateway {
	return &Gateway{
		Port:                "8090",
		BlueBubblesURL:      fx.server.URL,
		BlueBubblesPassword: "Password234!",
		WebhookPath:         "/bluebubbles/webhook",
		HTTPClient:          fx.server.Client(),
		Log:                 config.NewLogger(config.LevelError),
	}
}

func TestEnsureWebhookRegisteredSkipsWhenMatch(t *testing.T) {
	fx := newWebhookRegisterFixture(t,
		`{"status":200,"message":"ok","data":[{"id":1,"url":"http://127.0.0.1:8090/bluebubbles/webhook?password=Password234!","events":["new-message"]}]}`,
		http.StatusOK, http.StatusOK)
	g := fx.gateway()
	g.ensureWebhookRegistered()
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.getCount != 1 {
		t.Fatalf("expected one list request, got %d", fx.getCount)
	}
	if fx.postCount != 0 {
		t.Fatalf("expected no register POST for a matching webhook, got %d", fx.postCount)
	}
}

func TestEnsureWebhookRegisteredPostsRawPasswordCallback(t *testing.T) {
	fx := newWebhookRegisterFixture(t,
		`{"status":200,"message":"ok","data":[]}`,
		http.StatusOK, http.StatusOK)
	g := fx.gateway()
	g.ensureWebhookRegistered()
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.postCount != 1 {
		t.Fatalf("expected one register POST, got %d", fx.postCount)
	}
	got := fx.posts[0]
	wantURL := "http://127.0.0.1:8090/bluebubbles/webhook?password=Password234!"
	if got.URL != wantURL {
		t.Fatalf("register URL = %q, want %q", got.URL, wantURL)
	}
	if len(got.Events) != 1 || got.Events[0] != "new-message" {
		t.Fatalf("register events = %v, want [new-message]", got.Events)
	}
}

func TestEnsureWebhookRegisteredPostsForStaleHost(t *testing.T) {
	fx := newWebhookRegisterFixture(t,
		`{"status":200,"message":"ok","data":[{"id":2,"url":"http://192.168.4.235:8090/bluebubbles/webhook?password=Password234!","events":["new-message"]}]}`,
		http.StatusOK, http.StatusOK)
	g := fx.gateway()
	g.ensureWebhookRegistered()
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.postCount != 1 {
		t.Fatalf("expected register POST for a stale-host webhook, got %d", fx.postCount)
	}
	if !strings.HasPrefix(fx.posts[0].URL, "http://127.0.0.1:8090/bluebubbles/webhook") {
		t.Fatalf("unexpected register URL %q", fx.posts[0].URL)
	}
}

func TestEnsureWebhookRegisteredFailOpen(t *testing.T) {
	listFail := newWebhookRegisterFixture(t, `server error`, http.StatusInternalServerError, http.StatusOK)
	listFail.gateway().ensureWebhookRegistered()
	listFail.mu.Lock()
	listPosts := listFail.postCount
	listFail.mu.Unlock()
	if listPosts != 0 {
		t.Fatalf("expected no register POST after list failure, got %d", listPosts)
	}

	postFail := newWebhookRegisterFixture(t, `{"status":200,"message":"ok","data":[]}`, http.StatusOK, http.StatusInternalServerError)
	postFail.gateway().ensureWebhookRegistered()

	unresolvable := &Gateway{
		Port:                "8090",
		BlueBubblesURL:      "",
		BlueBubblesPassword: "Password234!",
		WebhookPath:         "/bluebubbles/webhook",
		Log:                 config.NewLogger(config.LevelError),
	}
	unresolvable.ensureWebhookRegistered()
}

func TestWebhookCallbackMatches(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		ip    string
		port  string
		path  string
		match bool
	}{
		{"raw password", "http://127.0.0.1:8090/bluebubbles/webhook?password=Password234!", "127.0.0.1", "8090", "/bluebubbles/webhook", true},
		{"encoded password", "http://127.0.0.1:8090/bluebubbles/webhook?password=Password234%21", "127.0.0.1", "8090", "/bluebubbles/webhook", true},
		{"stale host", "http://192.168.4.235:8090/bluebubbles/webhook?password=Password234!", "127.0.0.1", "8090", "/bluebubbles/webhook", false},
		{"other path", "http://127.0.0.1:8090/bluebubbles-webhook?password=Password234!", "127.0.0.1", "8090", "/bluebubbles/webhook", false},
		{"other port", "http://127.0.0.1:8645/bluebubbles/webhook?password=Password234!", "127.0.0.1", "8090", "/bluebubbles/webhook", false},
		{"garbage", "://not a url", "127.0.0.1", "8090", "/bluebubbles/webhook", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhookCallbackMatches(tc.raw, tc.ip, tc.port, tc.path); got != tc.match {
				t.Fatalf("webhookCallbackMatches(%q) = %v, want %v", tc.raw, got, tc.match)
			}
		})
	}
}
