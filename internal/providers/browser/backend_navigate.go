package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/browser/safety"
	_ "image/png"
	"strings"
	"time"
	"unicode/utf8"
)

func (b *LocalBackend) Navigate(ctx context.Context, profile, rawURL string) (NavigateData, error) {
	canonical, err := safety.ValidateNavigateURL(rawURL, b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs)
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
	if err := safety.CheckFinalURL(finalURL, b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs); err != nil {
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
		URL:            safety.RedactSecrets(finalURL),
		Title:          safety.RedactSecrets(title),
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
	if err := safety.CheckFinalURL(finalURL, b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs); err != nil {
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
	return NavigateData{URL: safety.RedactSecrets(finalURL), Title: safety.RedactSecrets(title), Snapshot: snapshot, ElementCount: len(elements), PendingDialogs: pendingList(pt), Partial: partial}, nil
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
	dialog.Message = safety.RedactSecrets(dialog.Message)
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
