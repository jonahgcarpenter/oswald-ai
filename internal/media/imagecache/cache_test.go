package imagecache

import (
	"bytes"
	"context"
	"crypto/tls"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

func samplePNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSaveResolveSweepAndDelete(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "data")
	c := New(root)
	now := time.Now().Add(time.Second)
	c.now = func() time.Time { return now }
	data := samplePNG(t)
	path, err := c.Save(ctx, "alice", data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || !strings.HasPrefix(path, filepath.Join(root, "alice", ".cache", "images")+string(os.PathSeparator)) {
		t.Fatalf("wrong path: %q", path)
	}
	got, mime, err := c.Resolve(ctx, "alice", path)
	if err != nil || mime != "image/png" || !bytes.Equal(got, data) {
		t.Fatalf("resolve: %q %v", mime, err)
	}
	for _, user := range []string{"bob", "../alice"} {
		if _, _, err := c.Resolve(ctx, user, path); err == nil {
			t.Fatalf("resolved for %q", user)
		}
	}
	if _, err := c.Save(ctx, "alice", data, "image/jpeg"); err == nil {
		t.Fatal("accepted mismatched MIME")
	}
	if _, err := c.Save(ctx, "alice", []byte("not an image"), ""); err == nil {
		t.Fatal("accepted invalid image")
	}
	other := filepath.Join(root, "alice", ".cache", "images", "unrelated.txt")
	if err := os.WriteFile(other, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now.Add(-24*time.Hour), now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Resolve(ctx, "alice", path); err == nil {
		t.Fatal("resolved expired image")
	}
	counts, err := c.Sweep(ctx, now)
	if err != nil || counts.RemovedFiles != 1 || counts.RemovedBytes != int64(len(data)) {
		t.Fatalf("sweep: %+v %v", counts, err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal(err)
	}
	path, err = c.Save(ctx, "alice", data, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("image not deleted: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinkPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	c := New(root)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Save(ctx, "alice", samplePNG(t), ""); err == nil {
		t.Fatal("followed user directory symlink")
	}
	if _, err := c.Sweep(ctx, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	linkedRoot := New(filepath.Join(root, "linked"))
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := linkedRoot.Sweep(ctx, time.Now()); err == nil {
		t.Fatal("followed cache root symlink")
	}
	path, err := c.Save(ctx, "bob", samplePNG(t), "")
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), strings.Repeat("a", 32)+".png")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Resolve(ctx, "bob", link); err == nil {
		t.Fatal("followed image symlink")
	}
	if err := c.DeleteUser(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("deleted unrelated symlink:", err)
	}
}

func TestImportHTTPS(t *testing.T) {
	data := samplePNG(t)
	var secretSeen bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			secretSeen = true
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "https://other.example/image", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}))
	defer srv.Close()
	c := New(t.TempDir())
	lookup := func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host == "private.example" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	tlsConfig := &tls.Config{InsecureSkipVerify: true} // Only the local test server has a self-signed certificate.
	path, got, mime, err := c.importHTTPS(context.Background(), "alice", "https://example.com/start?token=private", lookup, dial, tlsConfig)
	if err != nil || mime != "image/png" || !bytes.Equal(data, got) || path == "" || secretSeen {
		t.Fatalf("import: %q %v", mime, err)
	}
	for _, raw := range []string{"http://example.com/image", "file:///image", "https://user:pass@example.com/image", "https://example.com/image#fragment", "https://private.example/image"} {
		if _, _, _, err := c.importHTTPS(context.Background(), "alice", raw, lookup, dial, tlsConfig); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	queries := 0
	rebind := func(_ context.Context, _, _ string) ([]netip.Addr, error) {
		queries++
		if queries > 1 {
			return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	if _, _, _, err := c.importHTTPS(context.Background(), "alice", "https://example.com/start", rebind, dial, tlsConfig); err == nil || queries != 2 {
		t.Fatalf("redirect did not revalidate DNS: %d %v", queries, err)
	}
}

func TestSaveAssetReuseExpiryAndOwnership(t *testing.T) {
	ctx := context.Background()
	c := New(t.TempDir())
	now := time.Now().Add(time.Second)
	c.now = func() time.Time { return now }
	original := samplePNG(t)
	other := append([]byte(nil), original...)
	// An alternate valid image must not replace the cached original.
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 3, 3))
	img.Set(1, 1, color.RGBA{G: 255, A: 255})
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	other = b.Bytes()
	path, err := c.SaveAsset(ctx, "alice", "opaque-source-id", original, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.SaveAsset(ctx, "alice", "opaque-source-id", other, "image/png")
	if err != nil || again != path {
		t.Fatalf("unstable asset path: %q %v", again, err)
	}
	if strings.Contains(path, "opaque-source-id") {
		t.Fatal("asset ID exposed in path")
	}
	got, _, err := c.Resolve(ctx, "alice", path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("original overwritten: %v", err)
	}
	bob, err := c.SaveAsset(ctx, "bob", "opaque-source-id", other, "image/png")
	if err != nil || bob == path {
		t.Fatalf("cross-user path: %q %v", bob, err)
	}
	if _, _, err := c.Resolve(ctx, "bob", path); err == nil {
		t.Fatal("cross-user resolve")
	}
	if err := os.Chtimes(path, now.Add(-25*time.Hour), now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	again, err = c.SaveAsset(ctx, "alice", "opaque-source-id", other, "image/png")
	if err != nil || again != path {
		t.Fatalf("expiry repair changed path: %q %v", again, err)
	}
	got, _, err = c.Resolve(ctx, "alice", path)
	if err != nil || !bytes.Equal(got, other) {
		t.Fatalf("expiry repair failed: %v", err)
	}
	if err := c.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("asset survived deletion: %v", err)
	}
	if _, _, err := c.Resolve(ctx, "bob", bob); err != nil {
		t.Fatalf("deletion affected other owner: %v", err)
	}
	again, err = c.SaveAsset(ctx, "alice", "opaque-source-id", original, "image/png")
	if err != nil || again != path {
		t.Fatalf("missing asset not recreated at stable path: %q %v", again, err)
	}
	if _, err := c.SaveAsset(ctx, "alice", "", original, "image/png"); err == nil {
		t.Fatal("accepted empty asset ID")
	}
}

func TestSaveAssetConcurrent(t *testing.T) {
	c := New(t.TempDir())
	data := samplePNG(t)
	const workers = 24
	paths := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = c.SaveAsset(context.Background(), "alice", "shared-id", data, "")
		}(i)
	}
	wg.Wait()
	for i := range paths {
		if errs[i] != nil || paths[i] != paths[0] {
			t.Fatalf("concurrent save %d: %q %v", i, paths[i], errs[i])
		}
	}
	if got, _, err := c.Resolve(context.Background(), "alice", paths[0]); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("concurrent result: %v", err)
	}
}

func TestValidationAndUnsafeSweep(t *testing.T) {
	ctx := context.Background()
	c := New(t.TempDir())
	var b bytes.Buffer
	if err := gif.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatal(err)
	}
	gifPath, err := c.Save(ctx, "alice", b.Bytes(), "image/gif")
	if err != nil {
		t.Fatalf("valid GIF rejected: %v", err)
	}
	if _, mime, err := c.Resolve(ctx, "alice", gifPath); err != nil || mime != "image/gif" {
		t.Fatalf("GIF resolve: %q %v", mime, err)
	}
	big := append(samplePNG(t), make([]byte, media.MaxOutputAttachmentBytes)...)
	if err := os.WriteFile(gifPath, big, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Resolve(ctx, "alice", gifPath); err == nil {
		t.Fatal("resolved oversized image")
	}
	if err := os.Mkdir(filepath.Join(c.root, "bob"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(c.root, "bob", ".cache")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sweep(ctx, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("unsafe cache blocked sweep: %v", err)
	}
	if _, _, err := c.Resolve(ctx, "bob", filepath.Join(c.root, "bob", ".cache", "images", filepath.Base(gifPath))); err == nil {
		t.Fatal("resolved through symlink")
	}
	if err := os.Mkdir(filepath.Join(c.root, "carol"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(c.root, "carol", ".cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(c.root, "carol", ".cache", "images")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sweep(ctx, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("symlinked images directory blocked sweep: %v", err)
	}
	if _, err := c.SaveAsset(ctx, "carol", "asset", samplePNG(t), ""); err == nil {
		t.Fatal("saved through symlinked images directory")
	}
	assetPath, err := c.SaveAsset(ctx, "alice", "unsafe-asset", samplePNG(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(assetPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, samplePNG(t), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, assetPath); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SaveAsset(ctx, "alice", "unsafe-asset", samplePNG(t), ""); err == nil {
		t.Fatal("replaced symlinked asset")
	}
	if info, err := os.Lstat(assetPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink overwritten: %v", err)
	}
}
