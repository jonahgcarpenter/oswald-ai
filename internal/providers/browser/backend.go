package browser

import (
	"context"
	"fmt"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/browser/launcher"
	_ "image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
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
	launched    *launcher.LaunchedBrowser
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
	dial        func(ctx context.Context, sess *browserSession) (pageTransport, *launcher.LaunchedBrowser, error)
}

// NewLocalBackend builds a backend bound to one profile root for snapshot
// spill files. Pass the profile's root (cfg.ProfileRoot).
func NewLocalBackend(opts Options, profileRoot string) *LocalBackend {
	b := &LocalBackend{opts: opts.Effective(), profileRoot: profileRoot, sessions: map[string]*browserSession{}}
	b.dial = b.defaultDial
	return b
}

// NewLocalBackendWithDial substitutes transport creation for tests.
func NewLocalBackendWithDial(opts Options, profileRoot string, dial func(context.Context, *browserSession) (pageTransport, *launcher.LaunchedBrowser, error)) *LocalBackend {
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
	_, _, err := launcher.FindChromium(b.opts.LaunchCommand)
	return err == nil
}

func (b *LocalBackend) defaultDial(ctx context.Context, sess *browserSession) (pageTransport, *launcher.LaunchedBrowser, error) {
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
	binary, extra, err := launcher.FindChromium(b.opts.LaunchCommand)
	if err != nil {
		return nil, nil, err
	}
	launched, err := launcher.LaunchChromium(ctx, binary, extra, b.profileRoot)
	if err != nil {
		return nil, nil, err
	}
	pt, err := openPage(ctx, launched.URL, onLog, onErr)
	if err != nil {
		_ = launched.Cmd.Process.Kill()
		os.RemoveAll(launched.Dir)
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
		if sess.launched.Cmd != nil && sess.launched.Cmd.Process != nil {
			_ = sess.launched.Cmd.Process.Kill()
		}
		os.RemoveAll(sess.launched.Dir)
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
		if sess.launched != nil && sess.launched.Dir != "" {
			live[sess.launched.Dir] = true
		}
		sess.mu.Unlock()
		if idle > b.opts.InactivityTimeout || now.Sub(sess.lastActive) > 3600*time.Second {
			if sess.launched != nil && sess.launched.Dir != "" {
				delete(live, sess.launched.Dir)
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
	entries, err := os.ReadDir(launcher.SessionBaseDir(profileRoot))
	if err != nil {
		return 0
	}
	reaped := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "profile-") {
			continue
		}
		full := filepath.Join(launcher.SessionBaseDir(profileRoot), e.Name())
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
