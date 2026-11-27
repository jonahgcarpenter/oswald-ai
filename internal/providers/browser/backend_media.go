package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/browser/safety"
	"image"
	"image/color"
	"image/draw"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
			text = strings.TrimSpace(safety.RedactSecrets(text))
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
		sb.WriteString("Page: " + safety.RedactSecrets(pageURL) + "\n")
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
