package webfetch

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

func TestThumbnailProtectedDownloadPreservesQueryAndHasNoCredentials(t *testing.T) {
	var body bytes.Buffer
	if err := png.Encode(&body, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	const query = "url=https%3A%2F%2Fexample.com%2Fa%3Fx%3D1&width=500"
	client, _, dialed := mappedClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != query || r.Header.Get("X-Subscription-Token") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("query or credential boundary")
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body.Bytes())
	})
	transport := client.httpClient.Transport.(*http.Transport)
	if transport.Proxy != nil || !transport.DisableKeepAlives {
		t.Fatal("unsafe transport")
	}
	img, err := client.DownloadThumbnail(context.Background(), "http://public.example/p?"+query)
	if err != nil || img.Data == "" || len(*dialed) != 1 || (*dialed)[0] != "93.184.216.34:80" {
		t.Fatalf("err=%v dials=%v", err, *dialed)
	}
}

func TestThumbnailRejectsUnsafeRedirectsBodiesAndDNS(t *testing.T) {
	client, _, _ := mappedClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/private":
			http.Redirect(w, r, "http://127.0.0.1/private", 302)
		case "/loop":
			http.Redirect(w, r, "http://public.example/loop", 302)
		case "/large":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte(strings.Repeat("x", media.MaxSearchPreviewBytes+1)))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("not an image"))
		}
	})
	for _, path := range []string{"private", "loop", "large", "html"} {
		if _, err := client.DownloadThumbnail(context.Background(), "http://public.example/"+path); err == nil {
			t.Fatal("accepted", path)
		}
	}
	for _, mixed := range []bool{false, true} {
		client, _, dialed := mappedClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://second.example/p", 302) })
		resolve := &redirectResolver{after: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}
		if mixed {
			resolve.after = append(resolve.after, netip.MustParseAddr("93.184.216.35"))
		}
		transport := client.httpClient.Transport.(*http.Transport)
		transport.DialContext = secureDialContext(resolve, transport.DialContext)
		if _, err := client.DownloadThumbnail(context.Background(), "http://public.example/start"); err == nil || len(*dialed) != 1 {
			t.Fatal("unsafe DNS redirect dial")
		}
	}
}
