package imagecache

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestValidVideoURL(t *testing.T) {
	for _, raw := range []string{
		"https://public.example/clip.mp4",
		"https://public.example:8443/v/a.mov?x=1",
	} {
		if _, err := validVideoURL(raw); err != nil {
			t.Fatalf("rejected %q: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"http://public.example/clip.mp4",
		"https://user:pass@public.example/clip.mp4",
		"https://public.example/clip.mp4#t=1",
		"https://public.example:0/clip.mp4",
		"https://",
		"/local/clip.mp4",
	} {
		if _, err := validVideoURL(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestDownloadVideo(t *testing.T) {
	body := append([]byte("ftypmp42"), make([]byte, 1024)...)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/clip.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(body)
		case "/downgrade":
			http.Redirect(w, r, "http://public.example/clip.mp4", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>not a video</html>"))
		}
	}))
	defer srv.Close()
	lookup := func(_ context.Context, _, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Fatalf("dial not pinned to validated IP: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	got, mime, err := downloadVideo(context.Background(), "https://public.example/clip.mp4", lookup, dial, tlsConfig)
	if err != nil || mime != "video/mp4" || string(got) != string(body) {
		t.Fatalf("download mime=%s len=%d err=%v", mime, len(got), err)
	}
	if _, _, err := downloadVideo(context.Background(), "https://public.example/downgrade", lookup, dial, tlsConfig); err == nil {
		t.Fatal("HTTPS downgrade accepted")
	}
	if _, _, err := downloadVideo(context.Background(), "https://public.example/page", lookup, dial, tlsConfig); err == nil {
		t.Fatal("non-video content accepted")
	}
	if _, _, err := downloadVideo(context.Background(), "http://public.example/clip.mp4", lookup, dial, tlsConfig); err == nil {
		t.Fatal("HTTP video URL accepted")
	}
}
