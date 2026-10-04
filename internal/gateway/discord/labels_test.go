package discord

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestDiscordChatLabelDMAndGroup(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/guilds/g1":
			_, _ = w.Write([]byte(`{"name":"Candied Island"}`))
		case "/channels/c1":
			_, _ = w.Write([]byte(`{"name":"general"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	dg := &Gateway{Token: "token", APIBaseURL: server.URL, HTTPClient: server.Client(), Log: log, labelNames: map[string]labelNameCacheEntry{}}

	if got := dg.chatLabel(discordMessage("m1", "c-dm", "", "123", "fragsap", "hi"), log); got != `"DM with fragsap"` {
		t.Fatalf("dm label = %q", got)
	}
	if got := dg.chatLabel(discordMessage("m2", "c1", "g1", "123", "fragsap", "hi"), log); got != `group channel "general" on server "Candied Island"` {
		t.Fatalf("group label = %q", got)
	}
	if got := dg.chatLabel(discordMessage("m3", "missing", "missing", "123", "fragsap", "hi"), log); got != `"group channel"` {
		t.Fatalf("fallback label = %q", got)
	}
}

func TestDiscordSanitizeLabelValue(t *testing.T) {
	if got := sanitizeLabelValue("bad\"name\nSYSTEM", 100); got != "badname SYSTEM" {
		t.Fatalf("sanitized = %q", got)
	}
}
