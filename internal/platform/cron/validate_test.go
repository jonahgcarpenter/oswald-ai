package cron

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIMessageValidatorRejectsUnknownChat(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/api/v1/chat/") && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer server.Close()
	v := NewIMessageTargetValidator(server.URL, "pw")
	if err := v.ValidateTarget(context.Background(), "iMessage;+;chat5814039491040595", "+16626873509"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown chat accepted: %v (paths %v)", err, paths)
	}
}

func TestIMessageValidatorFailureModes(t *testing.T) {
	groupBody := `{"data":{"displayName":"Firebolt"}}`
	for _, tc := range []struct {
		name    string
		chat    string
		user    string
		chatOut int
		query   string
		want    string
	}{
		{name: "group ok no mention", chat: "iMessage;+;chat1", user: "", chatOut: 200, want: ""},
		{name: "group ok member", chat: "iMessage;+;chat1", user: "+16626873509", chatOut: 200, query: `{"data":[{"handle":{"address":"+16626873509"}}]}`, want: ""},
		{name: "group stranger mention", chat: "iMessage;+;chat1", user: "+19998887777", chatOut: 200, query: `{"data":[{"handle":{"address":"+16626873509"}}]}`, want: "not a participant"},
		{name: "group empty history fails open", chat: "iMessage;+;chat1", user: "+19998887777", chatOut: 200, query: `{"data":[]}`, want: ""},
		{name: "group query error fails open", chat: "iMessage;+;chat1", user: "+19998887777", chatOut: 200, query: "", want: ""},
		{name: "group forbidden", chat: "iMessage;+;chat1", user: "", chatOut: 403, want: "not accessible"},
		{name: "group server error fails closed", chat: "iMessage;+;chat1", user: "", chatOut: 500, want: "cannot be verified"},
		{name: "bare id rejected", chat: "firebolt-freezer", user: "", chatOut: 200, want: "not a chat GUID"},
		{name: "dm handle ok", chat: "dm", user: "+16626873509", chatOut: 200, want: ""},
		{name: "dm display name rejected", chat: "dm", user: "Jonah Carpenter", chatOut: 200, want: "phone number"},
		{name: "direct guid ok", chat: "iMessage;-;+16626873509", user: "", chatOut: 200, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/v1/message/query") {
					if tc.query == "" {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(tc.query))
					return
				}
				w.WriteHeader(tc.chatOut)
				_, _ = w.Write([]byte(groupBody))
			}))
			defer server.Close()
			v := NewIMessageTargetValidator(server.URL, "pw")
			err := v.ValidateTarget(context.Background(), tc.chat, tc.user)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	if err := NewIMessageTargetValidator("", "").ValidateTarget(context.Background(), "iMessage;+;chat1", ""); err == nil {
		t.Fatal("unconfigured validator accepted a target")
	}
	if err := NewIMessageTargetValidator("http://127.0.0.1:1", "pw").ValidateTarget(context.Background(), "iMessage;+;chat1", ""); err == nil {
		t.Fatal("unreachable server accepted a target")
	}
}

func discordValidatorServer(t *testing.T, channelOut int, channelBody string, memberOut int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users/@me/channels" && r.Method == http.MethodPost:
			var body struct {
				RecipientID string `json:"recipient_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.RecipientID == "123456789" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"dm-1"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/channels/"):
			w.WriteHeader(channelOut)
			_, _ = w.Write([]byte(channelBody))
		case strings.HasPrefix(r.URL.Path, "/guilds/"):
			w.WriteHeader(memberOut)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestDiscordValidatorFailureModes(t *testing.T) {
	guildChannel := `{"id":"chan1","type":0,"guild_id":"guild1"}`
	soloChannel := `{"id":"chan1","type":0,"guild_id":""}`
	for _, tc := range []struct {
		name       string
		chat       string
		user       string
		body       string
		channelOut int
		memberOut  int
		want       string
	}{
		{name: "channel ok no mention", chat: "chan1", user: "", body: soloChannel, channelOut: 200, memberOut: 200, want: ""},
		{name: "channel ok member", chat: "chan1", user: "123456789", body: guildChannel, channelOut: 200, memberOut: 200, want: ""},
		{name: "channel unknown", chat: "nope", user: "", body: guildChannel, channelOut: 404, memberOut: 200, want: "unknown"},
		{name: "channel forbidden", chat: "chan1", user: "", body: guildChannel, channelOut: 403, memberOut: 200, want: "not accessible"},
		{name: "channel error fails closed", chat: "chan1", user: "", body: guildChannel, channelOut: 500, memberOut: 200, want: "cannot be verified"},
		{name: "nonmember rejected", chat: "chan1", user: "999", body: guildChannel, channelOut: 200, memberOut: 404, want: "not a member"},
		{name: "dm user ok", chat: "dm", user: "123456789", body: guildChannel, channelOut: 200, memberOut: 200, want: ""},
		{name: "dm unknown user", chat: "dm", user: "000", body: guildChannel, channelOut: 200, memberOut: 200, want: "unknown"},
		{name: "dm missing user", chat: "dm", user: "", body: guildChannel, channelOut: 200, memberOut: 200, want: "required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := discordValidatorServer(t, tc.channelOut, tc.body, tc.memberOut)
			defer server.Close()
			v := NewDiscordTargetValidator(server.URL, "token")
			err := v.ValidateTarget(context.Background(), tc.chat, tc.user)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	if err := NewDiscordTargetValidator("", "").ValidateTarget(context.Background(), "chan1", ""); err == nil {
		t.Fatal("unconfigured validator accepted a target")
	}
}
