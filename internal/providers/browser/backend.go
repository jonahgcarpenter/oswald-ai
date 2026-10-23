package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/png"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// snapshotListJS returns interactive elements as JSON.
const snapshotListJS = `(() => { const els=[...document.querySelectorAll('a,button,input,select,textarea,[role="button"],[tabindex]')].filter(e=>{ try { const r=e.getBoundingClientRect(); const s=getComputedStyle(e); return s.visibility!=='hidden'&&s.display!=='none'&&(r.width>0||r.height>0||e===document.activeElement); } catch { return false; } }).slice(0,200); return JSON.stringify(els.map((e,i)=>{ let role=(e.getAttribute('role')||e.tagName||'').toLowerCase(); let name=((e.getAttribute('aria-label')||e.innerText||e.value||e.alt||e.title||'').trim().replace(/\s+/g,' ').slice(0,120)); return {ref:'@e'+(i+1),role:role,name:name}; })); })()`

// pageTextJS returns visible text for full snapshots.
const pageTextJS = `(() => { try { return document.body ? document.body.innerText.slice(0,60000) : ''; } catch { return ''; } })()`

// rectsJS returns bounding boxes for annotation.
const rectsJS = `(() => { const els=[...document.querySelectorAll('a,button,input,select,textarea,[role="button"],[tabindex]')].filter(e=>{ try { const r=e.getBoundingClientRect(); const s=getComputedStyle(e); return s.visibility!=='hidden'&&s.display!=='none'&&r.width>0&&r.height>0; } catch { return false; } }).slice(0,60); return JSON.stringify({viewport:{w:window.innerWidth,h:window.innerHeight}, boxes: els.map((e,i)=>{ const r=e.getBoundingClientRect(); return {ref:'@e'+(i+1),x:Math.round(r.x),y:Math.round(r.y),w:Math.round(r.width),h:Math.round(r.height)}; })}); })()`

// imagesJS lists non-data-URL images.
const imagesJS = `JSON.stringify([...document.images].slice(0,100).map(i=>({src:i.currentSrc||i.src||'',alt:i.alt||'',width:i.naturalWidth||0,height:i.naturalHeight||0})).filter(x=>x.src&&!/^\s*data:/i.test(x.src)))`

var refRE = regexp.MustCompile(`^@e(\d+)$`)

var allowedKeys = map[string]bool{
	"Enter": true, "Tab": true, "Escape": true, "Backspace": true,
	"Delete": true, "Home": true, "End": true, "PageUp": true, "PageDown": true,
	"ArrowUp": true, "ArrowDown": true, "ArrowLeft": true, "ArrowRight": true, " ": true, "Space": true,
}

var botTitleRE = regexp.MustCompile(`(?i)(captcha|verify you are human|are you a robot|access denied|cloudflare|attention required|bot|c Challenge)`)

// fullSnapshotCap caps the stored full tree at 2M chars.
const fullSnapshotCap = 2_000_000

// browserSession is one profile's live browser state.
type browserSession struct {
	profile     string
	transport   pageTransport
	launched    *launchedBrowser
	mu          sync.Mutex
	elements    []Element
	currentURL  string
	firstNav    bool
	lastActive  time.Time
	suspect     bool
	failCount   int
	consoleMsgs []ConsoleMessage
	jsErrors    []JSError
}

// LocalBackend drives per-profile Chromium sessions. Profiles are islands:
// sessions never cross profiles.
type LocalBackend struct {
	opts        Options
	profileRoot string
	mu          sync.Mutex
	sessions    map[string]*browserSession
	dial        func(ctx context.Context, sess *browserSession) (pageTransport, *launchedBrowser, error)
}

// NewLocalBackend builds a backend bound to one profile root for snapshot
// spill files. Pass the profile's root (cfg.ProfileRoot).
func NewLocalBackend(opts Options, profileRoot string) *LocalBackend {
	b := &LocalBackend{opts: opts.Effective(), profileRoot: profileRoot, sessions: map[string]*browserSession{}}
	b.dial = b.defaultDial
	return b
}

// NewLocalBackendWithDial substitutes transport creation for tests.
func NewLocalBackendWithDial(opts Options, profileRoot string, dial func(context.Context, *browserSession) (pageTransport, *launchedBrowser, error)) *LocalBackend {
	b := NewLocalBackend(opts, profileRoot)
	b.dial = dial
	return b
}

// Available reports whether a browser can be driven: an explicit endpoint or
// a discoverable Chromium binary. It performs no I/O.
func (b *LocalBackend) Available() bool {
	if strings.TrimSpace(b.opts.EndpointURL) != "" {
		return true
	}
	_, _, err := findChromium(b.opts.LaunchCommand)
	return err == nil
}

func (b *LocalBackend) defaultDial(ctx context.Context, sess *browserSession) (pageTransport, *launchedBrowser, error) {
	onLog := func(typ, text, source string) {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		if len(sess.consoleMsgs) < 200 {
			sess.consoleMsgs = append(sess.consoleMsgs, ConsoleMessage{Type: typ, Text: text, Source: source})
		}
	}
	onErr := func(message, source string) {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		if len(sess.jsErrors) < 200 {
			sess.jsErrors = append(sess.jsErrors, JSError{Message: message, Source: source})
		}
	}
	if endpoint := strings.TrimSpace(b.opts.EndpointURL); endpoint != "" {
		pt, err := openPage(ctx, endpoint, onLog, onErr)
		if err != nil {
			return nil, nil, err
		}
		return pt, nil, nil
	}
	binary, extra, err := findChromium(b.opts.LaunchCommand)
	if err != nil {
		return nil, nil, err
	}
	launched, err := launchChromium(ctx, binary, extra, b.profileRoot)
	if err != nil {
		return nil, nil, err
	}
	pt, err := openPage(ctx, launched.url, onLog, onErr)
	if err != nil {
		_ = launched.cmd.Process.Kill()
		os.RemoveAll(launched.dir)
		return nil, nil, err
	}
	return pt, launched, nil
}

// ensure returns the live session, recycling suspect or idle-expired entries
// before reuse and creating on first navigation.
func (b *LocalBackend) ensure(ctx context.Context, profile string, isOpen bool, firstNavPossible bool) (*browserSession, error) {
	b.mu.Lock()
	sess, ok := b.sessions[profile]
	if ok {
		sess.mu.Lock()
		idle := time.Since(sess.lastActive)
		suspect := sess.suspect
		sess.mu.Unlock()
		if suspect || idle > b.opts.InactivityTimeout {
			b.closeSessionLocked(sess)
			delete(b.sessions, profile)
			ok = false
		}
	}
	if !ok {
		sess = &browserSession{profile: profile}
		b.sessions[profile] = sess
	}
	b.mu.Unlock()

	sess.mu.Lock()
	needsDial := sess.transport == nil
	firstNav := !sess.firstNav && firstNavPossible
	sess.mu.Unlock()
	if !needsDial {
		return sess, nil
	}
	timeout := b.opts.CommandTimeout
	if isOpen {
		timeout = b.opts.OpenTimeout(firstNav)
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pt, launched, err := b.dial(dialCtx, sess)
	if err != nil {
		b.mu.Lock()
		if cur, ok := b.sessions[profile]; ok && cur == sess {
			delete(b.sessions, profile)
		}
		b.mu.Unlock()
		return nil, err
	}
	sess.mu.Lock()
	sess.transport = pt
	sess.launched = launched
	sess.lastActive = time.Now()
	sess.mu.Unlock()
	return sess, nil
}

// existing returns the session or an error when navigate has not run yet.
func (b *LocalBackend) existing(profile string) (*browserSession, error) {
	b.mu.Lock()
	sess, ok := b.sessions[profile]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("browser_navigate must be called first")
	}
	sess.mu.Lock()
	suspect := sess.suspect
	idle := time.Since(sess.lastActive)
	missing := sess.transport == nil || sess.currentURL == ""
	sess.mu.Unlock()
	if suspect || idle > b.opts.InactivityTimeout {
		b.recycle(profile, sess)
		return nil, fmt.Errorf("browser session expired; call browser_navigate again")
	}
	if missing {
		return nil, fmt.Errorf("browser_navigate must be called first")
	}
	return sess, nil
}

// recycle closes and drops a suspect or expired session.
func (b *LocalBackend) recycle(profile string, sess *browserSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur, ok := b.sessions[profile]; ok && cur == sess {
		b.closeSessionLocked(sess)
		delete(b.sessions, profile)
	}
}

func (b *LocalBackend) closeSessionLocked(sess *browserSession) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.transport != nil {
		_ = sess.transport.Close()
		sess.transport = nil
	}
	if sess.launched != nil {
		if sess.launched.cmd != nil && sess.launched.cmd.Process != nil {
			_ = sess.launched.cmd.Process.Kill()
		}
		os.RemoveAll(sess.launched.dir)
		sess.launched = nil
	}
}

// markSuspect flags the session for recycle before reuse. It performs no I/O.
func (b *LocalBackend) markSuspect(sess *browserSession) {
	sess.mu.Lock()
	sess.suspect = true
	sess.failCount++
	sess.mu.Unlock()
}

func (b *LocalBackend) touch(sess *browserSession) {
	sess.mu.Lock()
	sess.lastActive = time.Now()
	sess.failCount = 0
	sess.mu.Unlock()
}

// Sweep reaps idle sessions, day-old snapshot spills, and orphaned session
// dirs. It is the janitor entry point for the profile prune worker.
func (b *LocalBackend) Sweep(now time.Time) int {
	reaped := 0
	b.mu.Lock()
	live := make(map[string]bool, len(b.sessions))
	for profile, sess := range b.sessions {
		sess.mu.Lock()
		idle := now.Sub(sess.lastActive)
		if sess.launched != nil && sess.launched.dir != "" {
			live[sess.launched.dir] = true
		}
		sess.mu.Unlock()
		if idle > b.opts.InactivityTimeout || now.Sub(sess.lastActive) > 3600*time.Second {
			if sess.launched != nil && sess.launched.dir != "" {
				delete(live, sess.launched.dir)
			}
			b.closeSessionLocked(sess)
			delete(b.sessions, profile)
			reaped++
		}
	}
	b.mu.Unlock()
	reaped += sweepSnapshotSpills(b.profileRoot, now)
	reaped += sweepOrphanProfiles(b.profileRoot, live, now)
	return reaped
}

// sweepOrphanProfiles removes session dirs no live session owns, guarded by a
// 24h age so a sibling process's fresh dir is never touched.
func sweepOrphanProfiles(profileRoot string, live map[string]bool, now time.Time) int {
	if strings.TrimSpace(profileRoot) == "" {
		return 0
	}
	entries, err := os.ReadDir(sessionBaseDir(profileRoot))
	if err != nil {
		return 0
	}
	reaped := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "profile-") {
			continue
		}
		full := filepath.Join(sessionBaseDir(profileRoot), e.Name())
		if live[full] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < 24*time.Hour {
			continue
		}
		if err := os.RemoveAll(full); err == nil {
			reaped++
		}
	}
	return reaped
}

// CloseAll terminates every session. It runs at process exit and in tests.
func (b *LocalBackend) CloseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for profile, sess := range b.sessions {
		b.closeSessionLocked(sess)
		delete(b.sessions, profile)
	}
}

func sweepSnapshotSpills(profileRoot string, now time.Time) int {
	dir := filepath.Join(profileRoot, "cache", "browser")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	reaped := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "scratch-") || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > 24*time.Hour {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				reaped++
			}
		}
	}
	return reaped
}

// Navigate opens a URL, initializing the profile session on first use.
func (b *LocalBackend) Navigate(ctx context.Context, profile, rawURL string) (NavigateData, error) {
	canonical, err := ValidateNavigateURL(rawURL, b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs)
	if err != nil {
		return NavigateData{}, err
	}
	sess, err := b.ensure(ctx, profile, true, true)
	if err != nil {
		return NavigateData{}, err
	}
	timeout := b.opts.OpenTimeout(true)
	sess.mu.Lock()
	first := !sess.firstNav
	sess.mu.Unlock()
	if !first {
		timeout = b.opts.OpenTimeout(false)
	}
	navCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sess.mu.Lock()
	pt := sess.transport
	sess.mu.Unlock()
	if pt == nil {
		return NavigateData{}, fmt.Errorf("browser session is not ready")
	}
	finalURL, title, err := pt.Navigate(navCtx, canonical)
	partial := false
	if err != nil {
		var partialErr *PartialPageError
		if errors.As(err, &partialErr) && navCtx.Err() != context.Canceled && ctx.Err() == nil {
			// The page never reached readiness but something loaded:
			// degrade to a partial snapshot instead of failing.
			partial = true
			finalURL, title = partialErr.URL, partialErr.Title
		} else {
			if navCtx.Err() != nil {
				b.markSuspect(sess)
			}
			return NavigateData{}, fmt.Errorf("browser navigate failed: %w", err)
		}
	}
	if finalURL == "" {
		finalURL = canonical
	}
	if err := CheckFinalURL(finalURL, b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs); err != nil {
		blankCtx, blankCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = pt.Blank(blankCtx)
		blankCancel()
		b.recycle(profile, sess)
		return NavigateData{}, err
	}
	elements, snapshot := b.refreshElements(ctx, sess, pt)
	sess.mu.Lock()
	sess.currentURL = finalURL
	sess.firstNav = true
	sess.suspect = false
	sess.lastActive = time.Now()
	sess.failCount = 0
	sess.mu.Unlock()
	return NavigateData{
		URL:            RedactSecrets(finalURL),
		Title:          RedactSecrets(title),
		Snapshot:       snapshot,
		ElementCount:   len(elements),
		PendingDialogs: pendingList(pt),
		BotWarning:     botTitleRE.MatchString(title),
		Partial:        partial,
	}, nil
}

// Snapshot renders the current accessibility tree.
func (b *LocalBackend) Snapshot(ctx context.Context, profile string, full bool) (SnapshotData, error) {
	sess, err := b.existing(profile)
	if err != nil {
		return SnapshotData{}, err
	}
	if err := b.guardCurrentURL(ctx, profile, sess); err != nil {
		return SnapshotData{}, err
	}
	sess.mu.Lock()
	pt := sess.transport
	sess.mu.Unlock()
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	elements, rendered := b.snapshotLocked(timeoutCtx, sess, pt, full)
	if rendered == "(snapshot unavailable)" {
		b.markSuspect(sess)
		return SnapshotData{}, fmt.Errorf("browser snapshot failed; call browser_navigate again")
	}
	b.touch(sess)
	if len([]rune(rendered)) <= b.opts.SnapshotThreshold {
		return SnapshotData{Snapshot: rendered, ElementCount: len(elements), PendingDialogs: pendingList(pt)}, nil
	}
	truncated := truncateLines(rendered, b.opts.SnapshotThreshold)
	path, spillErr := spillSnapshot(b.profileRoot, rendered)
	if spillErr != nil {
		return SnapshotData{Snapshot: truncated, ElementCount: len(elements), PendingDialogs: pendingList(pt), Truncated: true}, nil
	}
	nextLine := strings.Count(truncated, "\n") + 1
	truncated += fmt.Sprintf("\n[... full snapshot: read_file path=%q offset=%d limit=200]", path, nextLine)
	return SnapshotData{Snapshot: truncated, ElementCount: len(elements), PendingDialogs: pendingList(pt), Truncated: true, Path: path}, nil
}

// Click activates an element by ref.
func (b *LocalBackend) Click(ctx context.Context, profile, ref string) (SnapshotData, error) {
	idx, err := parseRef(ref)
	if err != nil {
		return SnapshotData{}, err
	}
	sess, err := b.existing(profile)
	if err != nil {
		return SnapshotData{}, err
	}
	if err := b.guardCurrentURL(ctx, profile, sess); err != nil {
		return SnapshotData{}, err
	}
	sess.mu.Lock()
	pt := sess.transport
	count := len(sess.elements)
	sess.mu.Unlock()
	if idx < 1 || idx > count {
		return SnapshotData{}, fmt.Errorf("unknown element ref %q", ref)
	}
	clickJS := fmt.Sprintf(`(() => { const els=[...document.querySelectorAll('a,button,input,select,textarea,[role="button"],[tabindex]')].filter(e=>{ try { const r=e.getBoundingClientRect(); const s=getComputedStyle(e); return s.visibility!=='hidden'&&s.display!=='none'&&(r.width>0||r.height>0); } catch { return false; } }); const e=els[%d]; if(!e) return 'missing'; e.scrollIntoView({block:'center'}); e.click(); return 'ok'; })()`, idx-1)
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	if _, err := pt.Evaluate(timeoutCtx, clickJS); err != nil {
		if timeoutCtx.Err() != nil {
			b.markSuspect(sess)
		}
		return SnapshotData{}, fmt.Errorf("browser click failed: %w", err)
	}
	b.touch(sess)
	return b.Snapshot(ctx, profile, false)
}

// Fill clears an input then types text.
func (b *LocalBackend) Fill(ctx context.Context, profile, ref, text string) (SnapshotData, error) {
	idx, err := parseRef(ref)
	if err != nil {
		return SnapshotData{}, err
	}
	if err := validateFillText(text); err != nil {
		return SnapshotData{}, err
	}
	sess, err := b.existing(profile)
	if err != nil {
		return SnapshotData{}, err
	}
	if err := b.guardCurrentURL(ctx, profile, sess); err != nil {
		return SnapshotData{}, err
	}
	sess.mu.Lock()
	pt := sess.transport
	count := len(sess.elements)
	sess.mu.Unlock()
	if idx < 1 || idx > count {
		return SnapshotData{}, fmt.Errorf("unknown element ref %q", ref)
	}
	encoded, _ := json.Marshal(text)
	fillJS := fmt.Sprintf(`(() => { const els=[...document.querySelectorAll('input,select,textarea')].concat([...document.querySelectorAll('[role="textbox"]')]); const all=[...document.querySelectorAll('a,button,input,select,textarea,[role="button"],[tabindex]')].filter(e=>{ try { const r=e.getBoundingClientRect(); const s=getComputedStyle(e); return s.visibility!=='hidden'&&s.display!=='none'&&(r.width>0||r.height>0); } catch { return false; } }); const e=all[%d]; if(!e) return 'missing'; e.focus(); const v=%s; if('setRangeText' in e && e.type!=='checkbox' && e.type!=='radio') { try { e.setRangeText(v,0,e.value.length,'end'); } catch { e.value=v; } } else if('value' in e) { e.value=v; } else { e.textContent=v; } e.dispatchEvent(new Event('input',{bubbles:true})); e.dispatchEvent(new Event('change',{bubbles:true})); return 'ok'; })()`, idx-1, string(encoded))
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	if _, err := pt.Evaluate(timeoutCtx, fillJS); err != nil {
		if timeoutCtx.Err() != nil {
			b.markSuspect(sess)
		}
		return SnapshotData{}, fmt.Errorf("browser type failed: %w", err)
	}
	b.touch(sess)
	return b.Snapshot(ctx, profile, false)
}

// Scroll moves the viewport half a screen up or down (~500px).
func (b *LocalBackend) Scroll(ctx context.Context, profile, direction string) (SnapshotData, error) {
	if direction != "up" && direction != "down" {
		return SnapshotData{}, fmt.Errorf("direction must be up or down")
	}
	sess, err := b.existing(profile)
	if err != nil {
		return SnapshotData{}, err
	}
	if err := b.guardCurrentURL(ctx, profile, sess); err != nil {
		return SnapshotData{}, err
	}
	delta := 500
	if direction == "up" {
		delta = -500
	}
	sess.mu.Lock()
	pt := sess.transport
	sess.mu.Unlock()
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	if _, err := pt.Evaluate(timeoutCtx, fmt.Sprintf(`window.scrollBy(0,%d); window.scrollY`, delta)); err != nil {
		if timeoutCtx.Err() != nil {
			b.markSuspect(sess)
		}
		return SnapshotData{}, fmt.Errorf("browser scroll failed: %w", err)
	}
	b.touch(sess)
	return b.Snapshot(ctx, profile, false)
}

// Back moves history back and rechecks the landed URL.
func (b *LocalBackend) Back(ctx context.Context, profile string) (NavigateData, error) {
	sess, err := b.existing(profile)
	if err != nil {
		return NavigateData{}, err
	}
	sess.mu.Lock()
	pt := sess.transport
	sess.mu.Unlock()
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	finalURL, title, err := pt.GoBack(timeoutCtx)
	partial := false
	if err != nil {
		var partialErr *PartialPageError
		if errors.As(err, &partialErr) && timeoutCtx.Err() != context.Canceled && ctx.Err() == nil {
			partial = true
			finalURL, title = partialErr.URL, partialErr.Title
		} else {
			if timeoutCtx.Err() != nil {
				b.markSuspect(sess)
			}
			return NavigateData{}, fmt.Errorf("browser back failed: %w", err)
		}
	}
	if err := CheckFinalURL(finalURL, b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs); err != nil {
		blankCtx, blankCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = pt.Blank(blankCtx)
		blankCancel()
		return NavigateData{}, err
	}
	elements, snapshot := b.refreshElements(ctx, sess, pt)
	sess.mu.Lock()
	sess.currentURL = finalURL
	sess.lastActive = time.Now()
	sess.mu.Unlock()
	return NavigateData{URL: RedactSecrets(finalURL), Title: RedactSecrets(title), Snapshot: snapshot, ElementCount: len(elements), PendingDialogs: pendingList(pt), Partial: partial}, nil
}

// pendingList reads the transport's blocking dialog, if any. Dialog message
// text is page content, so it passes through secret redaction.
func pendingList(pt pageTransport) []PendingDialog {
	if pt == nil {
		return nil
	}
	dialog, ok := pt.PendingDialog()
	if !ok {
		return nil
	}
	dialog.Message = RedactSecrets(dialog.Message)
	return []PendingDialog{dialog}
}

// RespondToDialog accepts or dismisses the native dialog blocking the page,
// then refreshes the snapshot.
func (b *LocalBackend) RespondToDialog(ctx context.Context, profile, action, promptText, dialogID string) (SnapshotData, error) {
	if action != "accept" && action != "dismiss" {
		return SnapshotData{}, fmt.Errorf("action must be accept or dismiss")
	}
	sess, err := b.existing(profile)
	if err != nil {
		return SnapshotData{}, err
	}
	if err := b.guardCurrentURL(ctx, profile, sess); err != nil {
		return SnapshotData{}, err
	}
	sess.mu.Lock()
	pt := sess.transport
	sess.mu.Unlock()
	if pending, ok := pt.PendingDialog(); ok && dialogID != "" && dialogID != pending.ID {
		return SnapshotData{}, fmt.Errorf("dialog_id %q not found", dialogID)
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	if err := pt.RespondDialog(timeoutCtx, action == "accept", promptText); err != nil {
		if timeoutCtx.Err() != nil {
			b.markSuspect(sess)
		}
		return SnapshotData{}, fmt.Errorf("browser dialog failed: %w", err)
	}
	b.touch(sess)
	return b.Snapshot(ctx, profile, false)
}

// Press dispatches a keyboard key.
func (b *LocalBackend) Press(ctx context.Context, profile, key string) (SnapshotData, error) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return SnapshotData{}, fmt.Errorf("key must not be empty")
	}
	if !allowedKeys[trimmed] && !(utf8.RuneCountInString(trimmed) == 1 && utf8.ValidString(trimmed) && trimmed[0] >= 32) {
		return SnapshotData{}, fmt.Errorf("unsupported key %q", trimmed)
	}
	sess, err := b.existing(profile)
	if err != nil {
		return SnapshotData{}, err
	}
	if err := b.guardCurrentURL(ctx, profile, sess); err != nil {
		return SnapshotData{}, err
	}
	sess.mu.Lock()
	pt := sess.transport
	sess.mu.Unlock()
	encoded, _ := json.Marshal(trimmed)
	pressJS := fmt.Sprintf(`(() => { const k=%s; const t=document.activeElement||document.body; const opts={key:k,bubbles:true,cancelable:true}; t.dispatchEvent(new KeyboardEvent('keydown',opts)); t.dispatchEvent(new KeyboardEvent('keypress',opts)); t.dispatchEvent(new KeyboardEvent('keyup',opts)); if(k==='Enter'){ const f=t&&t.form?t.form:(t&&t.closest?t.closest('form'):null); if(f&&f.requestSubmit){ try{f.requestSubmit();return 'submitted';}catch{} } } return 'ok'; })()`, string(encoded))
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	if _, err := pt.Evaluate(timeoutCtx, pressJS); err != nil {
		if timeoutCtx.Err() != nil {
			b.markSuspect(sess)
		}
		return SnapshotData{}, fmt.Errorf("browser press failed: %w", err)
	}
	b.touch(sess)
	return b.Snapshot(ctx, profile, false)
}

// Console returns buffered console output and JS errors.
func (b *LocalBackend) Console(ctx context.Context, profile string, clear bool) (ConsoleData, error) {
	sess, err := b.existing(profile)
	if err != nil {
		return ConsoleData{}, err
	}
	if err := b.guardCurrentURL(ctx, profile, sess); err != nil {
		return ConsoleData{}, err
	}
	if err := ctx.Err(); err != nil {
		return ConsoleData{}, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.lastActive = time.Now()
	msgs := append([]ConsoleMessage(nil), sess.consoleMsgs...)
	errs := append([]JSError(nil), sess.jsErrors...)
	out := ConsoleData{
		Messages:      RedactedConsole(msgs),
		Errors:        RedactedErrors(errs),
		TotalMessages: len(msgs),
		TotalErrors:   len(errs),
	}
	if out.Messages == nil {
		out.Messages = []ConsoleMessage{}
	}
	if out.Errors == nil {
		out.Errors = []JSError{}
	}
	if clear {
		sess.consoleMsgs = nil
		sess.jsErrors = nil
	}
	return out, nil
}

// Eval evaluates a JS expression with pre/post page-URL guards.
func (b *LocalBackend) Eval(ctx context.Context, profile, expression string) (EvalData, error) {
	return b.evalLocked(ctx, profile, expression, true)
}

// VaultEval evaluates trusted vault-built page scripts. It bypasses the
// restrict_evaluate denylist (classification and fill scripts legitimately
// touch inputs) but keeps every other guard: session ownership, live
// page-URL checks, timeouts, and the post-eval navigation recheck. The
// scripts are constructed server-side; only the secret payload they carry
// is sensitive, and it is never logged.
func (b *LocalBackend) VaultEval(ctx context.Context, profile, expression string) (string, error) {
	data, err := b.evalLocked(ctx, profile, expression, false)
	if err != nil {
		return "", err
	}
	return data.Result, nil
}

func (b *LocalBackend) evalLocked(ctx context.Context, profile, expression string, enforceDenylist bool) (EvalData, error) {
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" || len(trimmed) > 8000 || !utf8.ValidString(trimmed) {
		return EvalData{}, fmt.Errorf("expression must contain 1 to 8000 valid characters")
	}
	if blockedEvalLiteral(trimmed, b.opts.AllowPrivateURLs) {
		return EvalData{}, fmt.Errorf("expression targets a blocked URL")
	}
	if enforceDenylist && b.opts.RestrictEvaluate {
		if reason := sensitiveEvalReason(trimmed); reason != "" {
			return EvalData{}, fmt.Errorf("expression uses a sensitive browser primitive (%s) blocked by restrict_evaluate; use browser_snapshot or browser_console without expression for normal inspection", reason)
		}
	}
	sess, err := b.existing(profile)
	if err != nil {
		return EvalData{}, err
	}
	if err := b.guardCurrentURL(ctx, profile, sess); err != nil {
		return EvalData{}, err
	}
	sess.mu.Lock()
	pt := sess.transport
	sess.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return EvalData{}, err
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	result, err := pt.Evaluate(timeoutCtx, trimmed)
	if err != nil {
		if timeoutCtx.Err() != nil {
			b.markSuspect(sess)
		}
		return EvalData{}, err
	}
	b.touch(sess)
	// Post-eval page-URL recheck: navigate-then-read must not leak.
	if current, probeErr := pt.Evaluate(timeoutCtx, "location.href"); probeErr == nil {
		if err := CheckFinalURL(trimQuotes(current), b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs); err != nil {
			blankCtx, blankCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = pt.Blank(blankCtx)
			blankCancel()
			return EvalData{Withheld: true}, fmt.Errorf("evaluation landed on a blocked URL; result withheld")
		}
	}
	return EvalData{Result: RedactSecrets(result)}, nil
}

// Images lists non-data-URL page images.
func (b *LocalBackend) Images(ctx context.Context, profile string) (ImagesData, error) {
	sess, err := b.existing(profile)
	if err != nil {
		return ImagesData{}, err
	}
	if err := b.guardCurrentURL(ctx, profile, sess); err != nil {
		return ImagesData{}, err
	}
	sess.mu.Lock()
	pt := sess.transport
	sess.mu.Unlock()
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	raw, err := pt.Evaluate(timeoutCtx, imagesJS)
	if err != nil {
		if timeoutCtx.Err() != nil {
			b.markSuspect(sess)
		}
		return ImagesData{}, fmt.Errorf("browser images failed: %w", err)
	}
	var images []ImageInfo
	if err := json.Unmarshal([]byte(raw), &images); err != nil {
		return ImagesData{Images: []ImageInfo{}, Count: 0}, nil
	}
	b.touch(sess)
	return ImagesData{Images: images, Count: len(images)}, nil
}

// Screenshot captures the viewport, optionally annotating interactive boxes.
func (b *LocalBackend) Screenshot(ctx context.Context, profile string, annotate bool) (ScreenshotData, error) {
	sess, err := b.existing(profile)
	if err != nil {
		return ScreenshotData{}, err
	}
	if err := b.guardCurrentURL(ctx, profile, sess); err != nil {
		return ScreenshotData{}, err
	}
	sess.mu.Lock()
	pt := sess.transport
	sess.mu.Unlock()
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	png, err := pt.Screenshot(timeoutCtx)
	if err != nil {
		if timeoutCtx.Err() != nil {
			b.markSuspect(sess)
		}
		return ScreenshotData{}, fmt.Errorf("browser screenshot failed: %w", err)
	}
	b.touch(sess)
	if !annotate {
		return ScreenshotData{PNG: png}, nil
	}
	boxes, viewport, err := b.readBoxes(timeoutCtx, pt)
	if err != nil || len(boxes) == 0 {
		return ScreenshotData{PNG: png}, nil
	}
	return ScreenshotData{PNG: overlayBoxes(png, boxes, viewport)}, nil
}

// guardCurrentURL enforces read/input guards on the live page URL. The stored
// URL is checked first; the live location.href is then probed so an eval
// that moved the page to a private address cannot be read from afterwards.
// The probe fails open only when the probe itself is unusable (empty
// result); a confirmed private page is parked on about:blank and the
// session recycled so later calls cannot read internal content.
func (b *LocalBackend) guardCurrentURL(ctx context.Context, profile string, sess *browserSession) error {
	sess.mu.Lock()
	current := sess.currentURL
	pt := sess.transport
	sess.mu.Unlock()
	if strings.TrimSpace(current) == "" {
		return nil
	}
	if err := CheckFinalURL(current, b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs); err != nil {
		b.parkBlocked(profile, sess, pt)
		return err
	}
	if pt == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	live, err := pt.Evaluate(probeCtx, "location.href")
	if err != nil {
		return nil
	}
	live = trimQuotes(strings.TrimSpace(live))
	if live == "" || live == current {
		return nil
	}
	if err := CheckFinalURL(live, b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs); err != nil {
		b.parkBlocked(profile, sess, pt)
		return err
	}
	return nil
}

// parkBlocked moves a blocked page to about:blank and drops the session so
// no later call can read the internal content it navigated to.
func (b *LocalBackend) parkBlocked(profile string, sess *browserSession, pt pageTransport) {
	if pt != nil {
		blankCtx, blankCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = pt.Blank(blankCtx)
		blankCancel()
	}
	b.recycle(profile, sess)
}

func (b *LocalBackend) refreshElements(ctx context.Context, sess *browserSession, pt pageTransport) ([]Element, string) {
	timeoutCtx, cancel := context.WithTimeout(ctx, b.opts.CommandTimeout)
	defer cancel()
	raw, err := pt.Evaluate(timeoutCtx, snapshotListJS)
	if err != nil {
		if timeoutCtx.Err() != nil {
			b.markSuspect(sess)
		}
		return nil, "(snapshot unavailable)"
	}
	elements := parseElements(raw)
	sess.mu.Lock()
	sess.elements = elements
	sess.mu.Unlock()
	return elements, renderSnapshot(sessURL(sess), elements)
}

func (b *LocalBackend) snapshotLocked(ctx context.Context, sess *browserSession, pt pageTransport, full bool) ([]Element, string) {
	raw, err := pt.Evaluate(ctx, snapshotListJS)
	if err != nil {
		if ctx.Err() != nil {
			b.markSuspect(sess)
		}
		return nil, "(snapshot unavailable)"
	}
	elements := parseElements(raw)
	sess.mu.Lock()
	sess.elements = elements
	sess.mu.Unlock()
	rendered := renderSnapshot(sessURL(sess), elements)
	if full {
		text, err := pt.Evaluate(ctx, pageTextJS)
		if err == nil {
			if unquoted, uerr := unquoteJSString(text); uerr == nil {
				text = unquoted
			}
			text = strings.TrimSpace(RedactSecrets(text))
			if text != "" {
				rendered += "\n--- page text ---\n" + text
			}
		}
	}
	if len(rendered) > fullSnapshotCap {
		rendered = rendered[:fullSnapshotCap]
	}
	return elements, rendered
}

func sessURL(sess *browserSession) string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.currentURL
}

func parseElements(raw string) []Element {
	var items []Element
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
			trimmed = inner
		}
	}
	if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
		return nil
	}
	out := make([]Element, 0, len(items))
	for _, it := range items {
		if !refRE.MatchString(it.Ref) {
			continue
		}
		it.Role = strings.TrimSpace(it.Role)
		it.Name = strings.TrimSpace(it.Name)
		if it.Role == "" {
			continue
		}
		out = append(out, it)
	}
	return out
}

func renderSnapshot(pageURL string, elements []Element) string {
	var sb strings.Builder
	if pageURL != "" {
		sb.WriteString("Page: " + RedactSecrets(pageURL) + "\n")
	}
	if len(elements) == 0 {
		sb.WriteString("(no interactive elements)\n")
		return sb.String()
	}
	for _, e := range elements {
		sb.WriteString("[" + e.Ref + "] " + e.Role)
		if e.Name != "" {
			sb.WriteString(` "` + e.Name + `"`)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func truncateLines(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	cut := runes[:limit]
	last := -1
	for i, r := range string(cut) {
		_ = i
		if r == '\n' {
			last = i
		}
	}
	if last > 0 {
		return string(cut[:0]) + s[:last] + "\n...(truncated)"
	}
	return string(cut) + "\n...(truncated)"
}

func spillSnapshot(profileRoot, content string) (string, error) {
	if strings.TrimSpace(profileRoot) == "" {
		return "", fmt.Errorf("no profile root for snapshot spill")
	}
	if len(content) > fullSnapshotCap {
		content = content[:fullSnapshotCap]
	}
	dir := filepath.Join(profileRoot, "cache", "browser")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	// The content hash names the file so identical snapshots dedupe to one
	// spill. The scratch- file is model-readable through read_file, so the
	// truncation note can point the model at the full tree for paging.
	sum := sha256.Sum256([]byte(content))
	path := filepath.Join(dir, "scratch-"+hex.EncodeToString(sum[:])[:16]+".txt")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", err
	}
	return path, nil
}

func parseRef(ref string) (int, error) {
	m := refRE.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return 0, fmt.Errorf("ref must look like @e5")
	}
	var n int
	if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil || n < 1 {
		return 0, fmt.Errorf("ref must look like @e5")
	}
	return n, nil
}

func validateFillText(text string) error {
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return fmt.Errorf("text must be valid text")
	}
	trimmed := text
	if strings.TrimSpace(trimmed) == "" {
		return fmt.Errorf("text must not be empty")
	}
	if utf8.RuneCountInString(text) > 4000 {
		return fmt.Errorf("text exceeds 4000 characters")
	}
	return nil
}

// jsURLLiteralRE finds URL-shaped literals in JS expressions. Fetch/XHR
// calls and bare string navigations alike land here; a private-host fetch
// never updates location.href, so the post-eval page-URL recheck cannot see
// it and the literals are pre-screened instead.
var jsURLLiteralRE = regexp.MustCompile(`(?i)https?://[^\s'"` + "`" + `)\]<>]+`)

// blockedEvalLiteral pre-scans for private/metadata destinations embedded in
// a JS expression. Fail-open only via allowPrivate.
func blockedEvalLiteral(expr string, allowPrivate bool) bool {
	if allowPrivate {
		return false
	}
	for _, lit := range jsURLLiteralRE.FindAllString(expr, -1) {
		lit = strings.TrimRight(lit, ".,;")
		parsed, err := url.Parse(lit)
		if err != nil || parsed.Host == "" {
			continue
		}
		host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
		if host == "" {
			continue
		}
		if cloudMetadataHosts[host] || cloudMetadataHosts[strings.ToLower(strings.TrimSuffix(parsed.Host, "."))] {
			return true
		}
		if blocked, _ := isPrivateDestination(host); blocked {
			return true
		}
	}
	return false
}

// RedactedConsole redacts secret-pattern text for display.
func RedactedConsole(msgs []ConsoleMessage) []ConsoleMessage {
	out := make([]ConsoleMessage, 0, len(msgs))
	for _, m := range msgs {
		m.Text = RedactSecrets(m.Text)
		out = append(out, m)
	}
	return out
}

// RedactedErrors redacts secret-pattern error text.
func RedactedErrors(errs []JSError) []JSError {
	out := make([]JSError, 0, len(errs))
	for _, e := range errs {
		e.Message = RedactSecrets(e.Message)
		out = append(out, e)
	}
	return out
}

func unquoteJSString(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, `"`) {
		var out string
		if err := json.Unmarshal([]byte(trimmed), &out); err == nil {
			return out, nil
		}
	}
	return s, fmt.Errorf("not a quoted string")
}

func (b *LocalBackend) readBoxes(ctx context.Context, pt pageTransport) ([]box, viewport, error) {
	raw, err := pt.Evaluate(ctx, rectsJS)
	if err != nil {
		return nil, viewport{}, err
	}
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
			trimmed = inner
		}
	}
	var payload struct {
		Viewport viewport `json:"viewport"`
		Boxes    []box    `json:"boxes"`
	}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return nil, viewport{}, err
	}
	return payload.Boxes, payload.Viewport, nil
}

type box struct {
	Ref string `json:"ref"`
	X   int    `json:"x"`
	Y   int    `json:"y"`
	W   int    `json:"w"`
	H   int    `json:"h"`
}

type viewport struct {
	W int `json:"w"`
	H int `json:"h"`
}

// overlayBoxes draws red borders around interactive boxes. Without font
// support it draws boxes only; the [N] to @eN mapping rides in snapshot text.
func overlayBoxes(pngBytes []byte, boxes []box, vp viewport) []byte {
	img, _, err := image.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		// Byte-identical fallback: opaque CDP payloads stay untouched.
		return pngBytes
	}
	bounds := img.Bounds()
	out := image.NewNRGBA(bounds)
	draw.Draw(out, bounds, img, bounds.Min, draw.Src)
	sx, sy := 1.0, 1.0
	if vp.W > 0 {
		sx = float64(bounds.Dx()) / float64(vp.W)
	}
	if vp.H > 0 {
		sy = float64(bounds.Dy()) / float64(vp.H)
	}
	red := color.NRGBA{R: 255, A: 255}
	for _, bx := range boxes {
		x0 := bounds.Min.X + int(float64(bx.X)*sx)
		y0 := bounds.Min.Y + int(float64(bx.Y)*sy)
		x1 := x0 + max(1, int(float64(bx.W)*sx))
		y1 := y0 + max(1, int(float64(bx.H)*sy))
		strokeRect(out, x0, y0, x1, y1, red)
	}
	encoded, err := encodePNG(out)
	if err != nil {
		return pngBytes
	}
	return encoded
}
