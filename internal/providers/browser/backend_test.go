package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakePage struct {
	url          string
	title        string
	elements     string
	text         string
	images       string
	png          []byte
	partialNav   bool
	failElements bool
}

func (f *fakePage) Navigate(_ context.Context, url string) (string, string, error) {
	f.url = url
	if f.partialNav {
		return url, f.title, &PartialPageError{URL: url, Title: f.title}
	}
	return url, f.title, nil
}

func (f *fakePage) Evaluate(_ context.Context, expr string) (string, error) {
	if f.failElements && strings.Contains(expr, "JSON.stringify(els.map") {
		return "", fmt.Errorf("wedged renderer")
	}
	switch {
	case strings.Contains(expr, "querySelectorAll('a,button") && strings.Contains(expr, "JSON.stringify(els.map"):
		return f.elements, nil
	case strings.Contains(expr, "querySelectorAll('a,button") && strings.Contains(expr, "getBoundingClientRect"):
		return `{"viewport":{"w":800,"h":600},"boxes":[]}`, nil
	case strings.Contains(expr, "document.body") && strings.Contains(expr, "innerText"):
		return f.text, nil
	case strings.Contains(expr, "document.images"):
		return f.images, nil
	case strings.Contains(expr, "location.href"):
		return strconvQuote(f.url), nil
	case strings.Contains(expr, "document.title"):
		return strconvQuote(f.title), nil
	case strings.Contains(expr, "document.readyState"):
		return `"complete"`, nil
	case strings.Contains(expr, "window.scrollBy"):
		return "500", nil
	case strings.Contains(expr, "history.back"):
		f.url = "https://example.com/prev"
		return `"ok"`, nil
	case strings.Contains(expr, "KeyboardEvent"):
		return `"ok"`, nil
	case strings.Contains(expr, "setRangeText") || strings.Contains(expr, ".click()"):
		return `"ok"`, nil
	default:
		return `"ok"`, nil
	}
}

func strconvQuote(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

func (f *fakePage) Screenshot(_ context.Context) ([]byte, error) {
	if len(f.png) == 0 {
		return nil, fmt.Errorf("no screenshot")
	}
	return f.png, nil
}

func (f *fakePage) GoBack(_ context.Context) (string, string, error) {
	f.url = "https://example.com/prev"
	return f.url, f.title, nil
}

func (f *fakePage) Blank(_ context.Context) error { f.url = "about:blank"; return nil }
func (f *fakePage) Close() error                  { return nil }

func testBackend(pages map[string]*fakePage) *LocalBackend {
	return NewLocalBackendWithDial((Options{}).Effective(), "", func(_ context.Context, sess *browserSession) (pageTransport, *launchedBrowser, error) {
		fp := &fakePage{title: "Example", elements: `[{"ref":"@e1","role":"link","name":"Home"}]`, text: `"hello"`, images: `[]`, png: samplePNG()}
		if pages != nil {
			if p, ok := pages[sess.profile]; ok {
				return p, nil, nil
			}
		}
		_ = fp
		return &fakePage{title: "Example", elements: `[{"ref":"@e1","role":"link","name":"Home"}]`, text: `"hello"`, images: `[]`, png: samplePNG()}, nil, nil
	})
}

func samplePNG() []byte {
	// 1x1 transparent PNG.
	return []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 31, 21, 196, 137, 0, 0, 0, 13, 73, 68, 65, 84, 120, 156, 98, 96, 96, 96, 0, 0, 0, 4, 0, 1, 39, 160, 38, 11, 0, 0, 0, 0, 73, 69, 78, 68, 174, 66, 96, 130}
}

func TestNavigateRequiresOthersFirst(t *testing.T) {
	b := testBackend(nil)
	ctx := context.Background()
	if _, err := b.Snapshot(ctx, "alice", false); err == nil {
		t.Fatal("snapshot without navigate succeeded")
	}
	nav, err := b.Navigate(ctx, "alice", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if nav.ElementCount != 1 || !strings.Contains(nav.Snapshot, "@e1") {
		t.Fatalf("navigate = %+v", nav)
	}
	snap, err := b.Snapshot(ctx, "alice", false)
	if err != nil || snap.ElementCount != 1 {
		t.Fatalf("snapshot = %+v %v", snap, err)
	}
	if _, err := b.Click(ctx, "alice", "@e9"); err == nil {
		t.Fatal("click unknown ref succeeded")
	}
	if _, err := b.Click(ctx, "alice", "bogus"); err == nil {
		t.Fatal("click bad ref succeeded")
	}
	if _, err := b.Fill(ctx, "alice", "@e1", "  "); err == nil {
		t.Fatal("fill empty text succeeded")
	}
	console, err := b.Console(ctx, "alice", true)
	if err != nil || console.TotalMessages != 0 {
		t.Fatalf("console = %+v %v", console, err)
	}
	images, err := b.Images(ctx, "alice")
	if err != nil {
		_ = images
		t.Fatal(err)
	}
	shot, err := b.Screenshot(ctx, "alice", false)
	if err != nil || len(shot.PNG) == 0 {
		t.Fatalf("screenshot = %v %v", len(shot.PNG), err)
	}
	if _, err := b.Press(ctx, "alice", "Enter"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Press(ctx, "alice", "F99"); err == nil {
		t.Fatal("press bad key succeeded")
	}
	if _, err := b.Scroll(ctx, "alice", "sideways"); err == nil {
		t.Fatal("scroll bad direction succeeded")
	}
	back, err := b.Back(ctx, "alice")
	if err != nil || back.URL == "" {
		t.Fatalf("back = %+v %v", back, err)
	}
	if _, err := b.Eval(ctx, "alice", "document.title"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Eval(ctx, "alice", ""); err == nil {
		t.Fatal("empty eval succeeded")
	}
}

func TestPartialNavigateDegradesInsteadOfFailing(t *testing.T) {
	b := NewLocalBackendWithDial((Options{}).Effective(), "", func(_ context.Context, sess *browserSession) (pageTransport, *launchedBrowser, error) {
		return &fakePage{title: "Slow", elements: `[{"ref":"@e1","role":"link","name":"Home"}]`, text: `"hello"`, images: `[]`, png: samplePNG(), partialNav: true}, nil, nil
	})
	ctx := context.Background()
	nav, err := b.Navigate(ctx, "alice", "https://example.com/slow")
	if err != nil {
		t.Fatalf("partial navigate failed: %v", err)
	}
	if !nav.Partial || nav.ElementCount != 1 {
		t.Fatalf("partial navigate = %+v", nav)
	}
	snap, err := b.Snapshot(ctx, "alice", false)
	if err != nil || snap.ElementCount != 1 {
		t.Fatalf("snapshot after partial = %+v %v", snap, err)
	}
}

func TestSnapshotFailureMarksSuspect(t *testing.T) {
	pages := map[string]*fakePage{
		"alice": {title: "Example", elements: `[{"ref":"@e1","role":"link","name":"Home"}]`, text: `"hello"`, images: `[]`, png: samplePNG(), failElements: true},
	}
	b := testBackend(pages)
	ctx := context.Background()
	// Navigate degrades to an unavailable snapshot but still opens the page.
	nav, err := b.Navigate(ctx, "alice", "https://example.com")
	if err != nil {
		t.Fatalf("navigate with wedged renderer failed: %v", err)
	}
	if nav.Snapshot != "(snapshot unavailable)" {
		t.Fatalf("navigate snapshot = %q", nav.Snapshot)
	}
	// An explicit snapshot fails loudly and poisons the session.
	if _, err := b.Snapshot(ctx, "alice", false); err == nil {
		t.Fatal("snapshot with wedged renderer succeeded")
	}
	// The next use recycles the poisoned session instead of serving it.
	if _, err := b.Snapshot(ctx, "alice", false); err == nil {
		t.Fatal("snapshot on suspect session succeeded")
	} else if !strings.Contains(err.Error(), "browser_navigate") {
		t.Fatalf("suspect error = %v", err)
	}
}

func TestSuspectRecycleAndSweep(t *testing.T) {
	b := testBackend(nil)
	ctx := context.Background()
	if _, err := b.Navigate(ctx, "alice", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	sess := b.sessions["alice"]
	b.mu.Unlock()
	b.markSuspect(sess)
	if _, err := b.Navigate(ctx, "alice", "https://example.com"); err != nil {
		t.Fatalf("navigate after suspect failed: %v", err)
	}
	b.mu.Lock()
	sess = b.sessions["alice"]
	sess.lastActive = time.Now().Add(-2 * b.opts.InactivityTimeout)
	b.mu.Unlock()
	if got := b.Sweep(time.Now()); got < 1 {
		t.Fatalf("sweep reaped %d", got)
	}
}
