package imagecache

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

func TestPublicHTTPDownloadsPinDialingAndRevalidateRedirects(t *testing.T) {
	data := samplePNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("download forwarded private headers")
		}
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "http://other.example/image", http.StatusFound)
		case "/private":
			http.Redirect(w, r, "http://private.example/image", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "http://public.example/loop", http.StatusFound)
		case "/large":
			w.Header().Set("Content-Length", "9000000")
		case "/chunked":
			w.(http.Flusher).Flush()
			_, _ = w.Write(bytes.Repeat([]byte{'x'}, media.MaxOutputAttachmentBytes+1))
		case "/invalid":
			_, _ = w.Write([]byte("not an image"))
		default:
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(data)
		}
	}))
	defer srv.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	lookups, dials := 0, 0
	lookup := func(_ context.Context, _, host string) ([]netip.Addr, error) {
		lookups++
		if host == "private.example" {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		if address != "8.8.8.8:80" {
			t.Errorf("dial not pinned to validated IP: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	got, mime, err := downloadImage(context.Background(), "http://public.example/start?private=canary", true, lookup, dial, nil)
	if err != nil || mime != "image/png" || !bytes.Equal(got, data) || lookups != 2 || dials != 2 {
		t.Fatalf("download mime=%s lookups=%d dials=%d err=%v", mime, lookups, dials, err)
	}
	for _, path := range []string{"private", "loop", "large", "chunked", "invalid"} {
		if _, _, err := downloadImage(context.Background(), "http://public.example/"+path, true, lookup, dial, nil); err == nil {
			t.Fatalf("accepted %s response", path)
		}
	}
	for _, source := range []string{"file:///image", "http://user:pass@public.example/image", "http://public.example/image#fragment", "http://public.example:0/image", "http://public.example:65536/image"} {
		if _, _, err := downloadImage(context.Background(), source, true, lookup, dial, nil); err == nil {
			t.Fatal("accepted invalid URL")
		}
	}
	before := dials
	if _, _, err := downloadImage(context.Background(), "http://private.example/image", true, lookup, dial, nil); err == nil || dials != before {
		t.Fatal("private address reached dial")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := downloadImage(ctx, "http://public.example/image", true, lookup, dial, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("download cancellation=%v", err)
	}
}

func TestHTTPSOnlyImportStillRejectsHTTP(t *testing.T) {
	if _, _, err := downloadImage(context.Background(), "http://public.example/image", false, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "URL") {
		t.Fatal("HTTPS-only download accepted HTTP")
	}
}

func TestPublicDownloadRejectsHTTPSDowngrade(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://public.example/image", http.StatusFound)
	}))
	defer srv.Close()
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	dials := 0
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	// Only this local test server uses an untrusted test certificate.
	_, _, err := downloadImage(context.Background(), "https://public.example/start", true, lookup, dial, &tls.Config{InsecureSkipVerify: true})
	if err == nil || dials != 1 {
		t.Fatalf("HTTPS downgrade reached another dial: dials=%d err=%v", dials, err)
	}
}
