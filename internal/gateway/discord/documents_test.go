package discord

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCurrentDocumentsPartitionAndCDNRestriction(t *testing.T) {
	calls := 0
	g := &Gateway{HTTPClient: &http.Client{Transport: outboundTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("document"))}, nil
	})}}
	remaining, loader := g.currentDocuments([]Attachment{{ID: "1", Filename: "report.pdf", ContentType: "application/pdf", URL: "https://cdn.discordapp.com/attachments/report"}, {Filename: "image.png", ContentType: "image/png"}, {Filename: "song.mp3", ContentType: "audio/mpeg"}})
	if len(remaining) != 2 || loader == nil || calls != 0 {
		t.Fatal("incorrect current attachment partition")
	}
	if _, err := loader.Load(context.Background()); err != nil || calls != 1 {
		t.Fatalf("download=%v calls=%d", err, calls)
	}
	for _, u := range []string{"http://cdn.discordapp.com/a", "https://cdn.discordapp.com.attacker.test/a", "https://user:password@cdn.discordapp.com/a", "https://example.com/a"} {
		_, loader = g.currentDocuments([]Attachment{{Filename: "report.pdf", URL: u}})
		if _, err := loader.Load(context.Background()); err == nil {
			t.Fatalf("accepted %s", u)
		}
	}
	if calls != 1 {
		t.Fatal("untrusted URL reached transport")
	}
}
