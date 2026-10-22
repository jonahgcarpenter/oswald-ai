package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	dialogOpen   bool
	dialogMsg    string
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

func (f *fakePage) PendingDialog() (PendingDialog, bool) {
	if !f.dialogOpen {
		return PendingDialog{}, false
	}
	return PendingDialog{ID: "1", Type: "alert", Message: f.dialogMsg}, true
}

func (f *fakePage) RespondDialog(_ context.Context, _ bool, _ string) error {
	if !f.dialogOpen {
		return fmt.Errorf("no dialog is currently open")
	}
	f.dialogOpen = false
	return nil
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

func TestRestrictEvaluateDenylist(t *testing.T) {
	page := &fakePage{title: "Example", elements: `[{"ref":"@e1","role":"link","name":"Home"}]`, text: `"hello"`, images: `[]`, png: samplePNG()}
	newBackend := func(restrict bool) *LocalBackend {
		opts := (Options{RestrictEvaluate: restrict}).Effective()
		return NewLocalBackendWithDial(opts, "", func(_ context.Context, sess *browserSession) (pageTransport, *launchedBrowser, error) {
			return page, nil, nil
		})
	}
	ctx := context.Background()
	open := newBackend(false)
	if _, err := open.Navigate(ctx, "alice", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := open.Eval(ctx, "alice", "document.cookie"); err != nil {
		t.Fatalf("unrestricted eval blocked: %v", err)
	}
	locked := newBackend(true)
	if _, err := locked.Navigate(ctx, "alice", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.Eval(ctx, "alice", "document.cookie"); err == nil {
		t.Fatal("restricted eval allowed document.cookie")
	} else if !strings.Contains(err.Error(), "restrict_evaluate") {
		t.Fatalf("denylist error = %v", err)
	}
	if _, err := locked.Eval(ctx, "alice", "document.title"); err != nil {
		t.Fatalf("restricted eval blocked benign expression: %v", err)
	}
}

func TestLiveProbeParksPrivatePage(t *testing.T) {
	page := &fakePage{title: "Example", elements: `[{"ref":"@e1","role":"link","name":"Home"}]`, text: `"hello"`, images: `[]`, png: samplePNG()}
	b := testBackend(map[string]*fakePage{"alice": page})
	ctx := context.Background()
	if _, err := b.Navigate(ctx, "alice", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	// An eval that moves the page to a private address must not be readable.
	page.url = "http://127.0.0.1/evil"
	if _, err := b.Snapshot(ctx, "alice", false); err == nil {
		t.Fatal("snapshot on privately-navigated page succeeded")
	}
	if _, err := b.Snapshot(ctx, "alice", false); err == nil {
		t.Fatal("snapshot after park succeeded")
	} else if !strings.Contains(err.Error(), "browser_navigate") {
		t.Fatalf("park error = %v", err)
	}
}

func TestSpillIsHashedUnderScratch(t *testing.T) {
	root := t.TempDir()
	first, err := spillSnapshot(root, "snapshot content")
	if err != nil {
		t.Fatal(err)
	}
	second, err := spillSnapshot(root, "snapshot content")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("identical snapshots spilled twice: %q vs %q", first, second)
	}
	if !strings.Contains(first, filepath.Join("cache", "scratch", "browser-snapshots")) {
		t.Fatalf("spill not under scratch: %q", first)
	}
	other, err := spillSnapshot(root, "different content")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("distinct snapshots shared a spill file")
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(first, old, old); err != nil {
		t.Fatal(err)
	}
	b := NewLocalBackend((Options{}).Effective(), root)
	if got := b.Sweep(time.Now()); got < 1 {
		t.Fatalf("spill sweep reaped %d", got)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatal("old spill survived sweep")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("fresh spill was reaped")
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

func TestDialogRequiresOpenDialog(t *testing.T) {
	page := &fakePage{title: "Example", elements: `[{"ref":"@e1","role":"link","name":"Home"}]`, text: `"hello"`, images: `[]`, png: samplePNG()}
	b := testBackend(map[string]*fakePage{"alice": page})
	ctx := context.Background()
	if _, err := b.Navigate(ctx, "alice", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RespondToDialog(ctx, "alice", "accept", "", ""); err == nil {
		t.Fatal("dialog response without open dialog succeeded")
	}
	page.dialogOpen = true
	page.dialogMsg = "Confirm checkout?"
	snap, err := b.Snapshot(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.PendingDialogs) != 1 || snap.PendingDialogs[0].Message != "Confirm checkout?" {
		t.Fatalf("pending dialogs = %+v", snap.PendingDialogs)
	}
	if _, err := b.RespondToDialog(ctx, "alice", "accept", "", "9"); err == nil {
		t.Fatal("dialog response with wrong id succeeded")
	}
	if _, err := b.RespondToDialog(ctx, "alice", "maybe", "", ""); err == nil {
		t.Fatal("dialog response with bad action succeeded")
	}
	after, err := b.RespondToDialog(ctx, "alice", "accept", "", "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.PendingDialogs) != 0 {
		t.Fatalf("dialog still pending: %+v", after.PendingDialogs)
	}
}

func TestSnapshotTruncationPointsAtReadFile(t *testing.T) {
	var items strings.Builder
	items.WriteString("[")
	for i := 1; i <= 30; i++ {
		if i > 1 {
			items.WriteString(",")
		}
		fmt.Fprintf(&items, `{"ref":"@e%d","role":"link","name":"%s"}`, i, strings.Repeat("n", 100))
	}
	items.WriteString("]")
	root := t.TempDir()
	opts := Options{SnapshotThreshold: 1000}.Effective()
	page := &fakePage{title: "Example", elements: items.String(), text: `"hello"`, images: `[]`, png: samplePNG()}
	b := NewLocalBackendWithDial(opts, root, func(_ context.Context, sess *browserSession) (pageTransport, *launchedBrowser, error) {
		return page, nil, nil
	})
	ctx := context.Background()
	if _, err := b.Navigate(ctx, "alice", "https://example.com"); err != nil {
		t.Fatal(err)
	}
	snap, err := b.Snapshot(ctx, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Truncated || snap.Path == "" {
		t.Fatalf("oversized snapshot not spilled: %+v", len(snap.Snapshot))
	}
	if !strings.Contains(snap.Snapshot, "read_file path=") {
		t.Fatalf("truncation note lacks read_file pointer: %q", snap.Snapshot[len(snap.Snapshot)-200:])
	}
	stored, err := os.ReadFile(snap.Path)
	if err != nil {
		t.Fatalf("spill unreadable at %q: %v", snap.Path, err)
	}
	if !strings.Contains(string(stored), "@e30") {
		t.Fatal("spill is missing tail elements")
	}
}
