package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// pageTransport is the per-session CDP surface. cdpPage implements it against
// a live Chromium; tests substitute a fake.
type pageTransport interface {
	Navigate(ctx context.Context, url string) (finalURL, title string, err error)
	Evaluate(ctx context.Context, expression string) (string, error)
	Screenshot(ctx context.Context) ([]byte, error)
	GoBack(ctx context.Context) (finalURL, title string, err error)
	Blank(ctx context.Context) error
	Close() error
}

// cdpPage drives one DevTools page target.
type cdpPage struct {
	conn   *cdpConn
	onLog  func(consoleType, text, source string)
	onErr  func(message, source string)
	wsURL  string
	debug  string
	closed bool
}

// openPage dials endpointURL's first page target and enables console/exception
// events into onLog/onErr.
func openPage(ctx context.Context, endpointURL string, onLog func(string, string, string), onErr func(string, string)) (*cdpPage, error) {
	targets, err := listTargets(ctx, endpointURL)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no browser page target at %s", endpointURL)
	}
	p := &cdpPage{onLog: onLog, onErr: onErr, wsURL: targets[0], debug: endpointURL}
	conn, err := dialCDP(ctx, targets[0], p.handleEvent)
	if err != nil {
		return nil, err
	}
	p.conn = conn
	// Best-effort domain enable; a failure here degrades console buffers only.
	_ = p.enable(ctx)
	return p, nil
}

func (p *cdpPage) enable(ctx context.Context) error {
	for _, method := range []string{"Runtime.enable", "Page.enable", "Log.enable"} {
		if _, err := p.conn.Call(ctx, method, map[string]interface{}{}); err != nil {
			return err
		}
	}
	return nil
}

func listTargets(ctx context.Context, endpointURL string) ([]string, error) {
	base := endpointURL
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list browser targets: %w", err)
	}
	defer resp.Body.Close()
	var targets []struct {
		Type                 string `json:"type"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, fmt.Errorf("decode browser targets: %w", err)
	}
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			out = append(out, t.WebSocketDebuggerURL)
		}
	}
	return out, nil
}

func (p *cdpPage) handleEvent(method string, params json.RawMessage) {
	switch method {
	case "Runtime.consoleAPICalled":
		var evt struct {
			Type string `json:"type"`
			Args []struct {
				Value *string `json:"value"`
			} `json:"args"`
			StackTrace *struct {
				CallFrames []struct {
					URL string `json:"url"`
				} `json:"callFrames"`
			} `json:"stackTrace"`
		}
		if err := json.Unmarshal(params, &evt); err != nil || p.onLog == nil {
			return
		}
		text := ""
		for _, a := range evt.Args {
			if a.Value != nil {
				if text != "" {
					text += " "
				}
				text += *a.Value
			}
		}
		source := ""
		if evt.StackTrace != nil && len(evt.StackTrace.CallFrames) > 0 {
			source = evt.StackTrace.CallFrames[0].URL
		}
		p.onLog(evt.Type, text, source)
	case "Runtime.exceptionThrown":
		var evt struct {
			ExceptionDetails struct {
				Text string `json:"text"`
				URL  string `json:"url"`
			} `json:"exceptionDetails"`
		}
		if err := json.Unmarshal(params, &evt); err != nil || p.onErr == nil {
			return
		}
		p.onErr(evt.ExceptionDetails.Text, evt.ExceptionDetails.URL)
	case "Log.entryAdded":
		var evt struct {
			Entry struct {
				Level  string `json:"level"`
				Text   string `json:"text"`
				Source string `json:"source"`
				URL    string `json:"url"`
			} `json:"entry"`
		}
		if err := json.Unmarshal(params, &evt); err != nil {
			return
		}
		source := evt.Entry.Source
		if source == "" {
			source = evt.Entry.URL
		}
		if evt.Entry.Level == "error" {
			if p.onErr != nil {
				p.onErr(evt.Entry.Text, source)
			}
		} else if p.onLog != nil {
			p.onLog(evt.Entry.Level, evt.Entry.Text, source)
		}
	}
}

// evaluateRaw runs expression and returns the raw CDP value payload.
func (p *cdpPage) evaluateRaw(ctx context.Context, expression string, await bool) (json.RawMessage, error) {
	return p.conn.Call(ctx, "Runtime.evaluate", map[string]interface{}{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  await,
	})
}

// Evaluate runs JS and serializes the result: JSON strings parse through,
// other values marshal to JSON, strings pass through.
func (p *cdpPage) Evaluate(ctx context.Context, expression string) (string, error) {
	raw, err := p.evaluateRaw(ctx, expression, true)
	if err != nil {
		return "", rewriteEvalError(err)
	}
	var envelope struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("decode browser result: %w", err)
	}
	if envelope.ExceptionDetails != nil {
		return "", rewriteEvalError(fmt.Errorf("%s", envelope.ExceptionDetails.Text))
	}
	if len(envelope.Result.Value) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(envelope.Result.Value, &s); err == nil {
		var inner interface{}
		if err := json.Unmarshal([]byte(s), &inner); err == nil {
			compact, _ := json.Marshal(inner)
			if compact != nil {
				return string(compact), nil
			}
		}
		return s, nil
	}
	return string(envelope.Result.Value), nil
}

// PartialPageError reports a page that never reached readiness within the
// navigation timeout. It carries whatever the page shows so callers can
// degrade to a partial snapshot instead of failing.
type PartialPageError struct {
	URL   string
	Title string
}

// Error keeps the familiar readiness message.
func (e *PartialPageError) Error() string { return "browser page did not finish loading" }

// errPageNotReady is the internal readiness signal; Navigate and GoBack
// translate it into a *PartialPageError carrying page state.
var errPageNotReady = errors.New("browser page did not finish loading")

// interactiveGrace is how long waitReady insists on "complete" before also
// accepting "interactive" (usable DOM; deferred scripts may still run).
// Heavy bot-managed pages can sit in interactive indefinitely.
const interactiveGrace = 15 * time.Second

// captureTimeout bounds best-effort state capture after a readiness timeout.
const captureTimeout = 10 * time.Second

// Navigate opens url and waits for document readiness.
func (p *cdpPage) Navigate(ctx context.Context, url string) (string, string, error) {
	if _, err := p.conn.Call(ctx, "Page.navigate", map[string]interface{}{"url": url}); err != nil {
		return "", "", err
	}
	return p.finishNavigation(ctx)
}

// GoBack moves history back one entry.
func (p *cdpPage) GoBack(ctx context.Context) (string, string, error) {
	if _, err := p.Evaluate(ctx, "history.back()"); err != nil {
		return "", "", err
	}
	return p.finishNavigation(ctx)
}

// finishNavigation waits for readiness, then degrades to a partial result
// carrying current page state instead of failing when the timeout expires.
// User cancellation still returns an error with no capture.
func (p *cdpPage) finishNavigation(ctx context.Context) (string, string, error) {
	if err := p.waitReady(ctx); err == nil {
		finalURL, _ := p.Evaluate(ctx, "location.href")
		title, _ := p.Evaluate(ctx, "document.title")
		return trimQuotes(finalURL), trimQuotes(title), nil
	} else if !errors.Is(err, errPageNotReady) {
		return "", "", err
	}
	if ctx.Err() == context.Canceled {
		return "", "", ctx.Err()
	}
	capCtx, cancel := context.WithTimeout(context.Background(), captureTimeout)
	defer cancel()
	finalURL, _ := p.Evaluate(capCtx, "location.href")
	title, _ := p.Evaluate(capCtx, "document.title")
	finalURL, title = trimQuotes(finalURL), trimQuotes(title)
	return finalURL, title, &PartialPageError{URL: finalURL, Title: title}
}

// Blank parks the page where snapshots cannot read internal content.
func (p *cdpPage) Blank(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := p.conn.Call(ctx, "Page.navigate", map[string]interface{}{"url": "about:blank"})
	return err
}

// Screenshot captures a PNG of the current viewport.
func (p *cdpPage) Screenshot(ctx context.Context) ([]byte, error) {
	raw, err := p.conn.Call(ctx, "Page.captureScreenshot", map[string]interface{}{"format": "png"})
	if err != nil {
		return nil, err
	}
	var result struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Data == "" {
		return nil, fmt.Errorf("decode browser screenshot: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("decode browser screenshot: %w", err)
	}
	return data, nil
}

// Close terminates the debugger connection (the Chromium process is owned by
// the launcher, not the page).
func (p *cdpPage) Close() error {
	p.closed = true
	if p.conn == nil {
		return nil
	}
	return p.conn.Close()
}

// waitReady polls document.readyState until the caller's context expires, so
// the wait is bound by the navigation timeout (60s, 120s on first open).
// It succeeds on "complete" and, after interactiveGrace, on "interactive".
// Expiry yields errPageNotReady; genuine evaluation failures pass through.
func (p *cdpPage) waitReady(ctx context.Context) error {
	start := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return errPageNotReady
		}
		state, err := p.Evaluate(ctx, "document.readyState")
		if err != nil {
			if ctx.Err() != nil {
				return errPageNotReady
			}
			return err
		}
		switch trimQuotes(state) {
		case "complete":
			return nil
		case "interactive":
			if time.Since(start) >= interactiveGrace {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errPageNotReady
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var out string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
		return s[1 : len(s)-1]
	}
	return s
}

func rewriteEvalError(err error) error {
	msg := err.Error()
	switch {
	case containsFold(msg, "unsupported") || containsFold(msg, "not supported"):
		return fmt.Errorf("browser evaluation failed: %w (check the expression uses page DOM, not host features)", err)
	case containsFold(msg, "live") || containsFold(msg, "node") || containsFold(msg, "could not serialize"):
		return fmt.Errorf("browser evaluation failed: %w (return JSON-serializable values, not live DOM nodes)", err)
	default:
		return fmt.Errorf("browser evaluation failed: %w", err)
	}
}

func containsFold(haystack, needle string) bool {
	if len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			a, b := haystack[i+j], needle[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

var _ = os.Getenv
var _ = exec.Command
