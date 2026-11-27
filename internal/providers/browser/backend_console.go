package browser

import (
	"context"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/browser/safety"
	_ "image/png"
	"time"
)

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

func RedactedConsole(msgs []ConsoleMessage) []ConsoleMessage {
	out := make([]ConsoleMessage, 0, len(msgs))
	for _, m := range msgs {
		m.Text = safety.RedactSecrets(m.Text)
		out = append(out, m)
	}
	return out
}

// RedactedErrors redacts secret-pattern error text.

func RedactedErrors(errs []JSError) []JSError {
	out := make([]JSError, 0, len(errs))
	for _, e := range errs {
		e.Message = safety.RedactSecrets(e.Message)
		out = append(out, e)
	}
	return out
}
