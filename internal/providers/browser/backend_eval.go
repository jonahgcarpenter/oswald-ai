package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/browser/safety"
	_ "image/png"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

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
		if reason := safety.SensitiveEvalReason(trimmed); reason != "" {
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
		if err := safety.CheckFinalURL(trimQuotes(current), b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs); err != nil {
			blankCtx, blankCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = pt.Blank(blankCtx)
			blankCancel()
			return EvalData{Withheld: true}, fmt.Errorf("evaluation landed on a blocked URL; result withheld")
		}
	}
	return EvalData{Result: safety.RedactSecrets(result)}, nil
}

// Images lists non-data-URL page images.

func (b *LocalBackend) guardCurrentURL(ctx context.Context, profile string, sess *browserSession) error {
	sess.mu.Lock()
	current := sess.currentURL
	pt := sess.transport
	sess.mu.Unlock()
	if strings.TrimSpace(current) == "" {
		return nil
	}
	if err := safety.CheckFinalURL(current, b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs); err != nil {
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
	if err := safety.CheckFinalURL(live, b.opts.AllowPrivateURLs, b.opts.AllowInsecureURLs); err != nil {
		b.parkBlocked(profile, sess, pt)
		return err
	}
	return nil
}

// parkBlocked moves a blocked page to about:blank and drops the session so
// no later call can read the internal content it navigated to.

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
		if safety.IsCloudMetadataHost(host) || safety.IsCloudMetadataHost(strings.ToLower(strings.TrimSuffix(parsed.Host, "."))) {
			return true
		}
		if blocked, _ := safety.IsPrivateDestination(host); blocked {
			return true
		}
	}
	return false
}

// RedactedConsole redacts secret-pattern text for display.

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
