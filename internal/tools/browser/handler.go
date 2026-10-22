package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	providerbrowser "github.com/jonahgcarpenter/oswald-ai/internal/providers/browser"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media/imagecache"
)

// Deps wires one profile's browser backend to model handlers.
type Deps struct {
	Backend providerbrowser.Backend
	Cache   *imagecache.Cache
	Log     *config.Logger
}

// NewHandlers returns the ten browser handlers keyed by tool name.
func NewHandlers(deps Deps) map[string]registry.Handler {
	return map[string]registry.Handler{
		NavigateName:  navigateHandler(deps),
		SnapshotName:  snapshotHandler(deps),
		ClickName:     clickHandler(deps),
		TypeName:      typeHandler(deps),
		ScrollName:    scrollHandler(deps),
		BackName:      backHandler(deps),
		PressName:     pressHandler(deps),
		GetImagesName: getImagesHandler(deps),
		VisionName:    visionHandler(deps),
		ConsoleName:   consoleHandler(deps),
	}
}

func profileFromContext(ctx context.Context) (string, error) {
	principal, ok := requestctx.PrincipalFromContext(ctx)
	if !ok || !principal.Authenticated() {
		return "", errors.New("browser tools require an authenticated request")
	}
	return principal.CanonicalUserID, nil
}

func rejectUnknownArgs(args map[string]interface{}, allowed ...string) error {
	allowedSet := map[string]bool{}
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for key := range args {
		if !allowedSet[key] {
			return fmt.Errorf("unsupported browser argument %q", key)
		}
	}
	return nil
}

func stringArg(args map[string]interface{}, key string) (string, bool) {
	raw, ok := args[key].(string)
	return raw, ok
}

func boolArg(args map[string]interface{}, key string, def bool) (bool, error) {
	raw, exists := args[key]
	if !exists {
		return def, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return value, nil
}

func navigateHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.navigate", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, elementCount, snapshotChars, urlChars int) {
			agentLog.Debug("agent.tool.browser.navigate.complete", "browser navigate completed",
				config.F("tool_name", NavigateName), config.F("status", status), config.F("outcome", outcome),
				config.F("element_count", elementCount), config.F("snapshot_chars", snapshotChars),
				config.F("url_chars", urlChars), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0, 0, 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args, "url"); err != nil {
			completed("rejected", "rejected", 0, 0, 0)
			return governance.Result{}, err
		}
		rawURL, ok := stringArg(args, "url")
		if !ok || strings.TrimSpace(rawURL) == "" || !utf8.ValidString(rawURL) || len(rawURL) > 2048 {
			completed("rejected", "rejected", 0, 0, len([]rune(rawURL)))
			return governance.Result{}, errors.New("url must be a non-empty string up to 2048 characters")
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0, 0, len([]rune(rawURL)))
			return governance.Result{}, err
		}
		agentLog.Debug("agent.tool.browser.navigate.start", "starting browser navigate",
			config.F("tool_name", NavigateName), config.F("url_chars", len([]rune(rawURL))))
		data, err := deps.Backend.Navigate(ctx, profile, rawURL)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				completed("error", "error", 0, 0, len([]rune(rawURL)))
				return governance.Result{}, fmt.Errorf("browser navigate canceled: %w", ctxErr)
			}
			completed("error", "error", 0, 0, len([]rune(rawURL)))
			return governance.Result{}, err
		}
		payload := map[string]interface{}{
			"url": data.URL, "title": data.Title, "snapshot": data.Snapshot, "element_count": data.ElementCount,
		}
		if data.BotWarning {
			payload["bot_detection_warning"] = "Page title '" + data.Title + "' suggests bot detection. The site may have blocked this request. " +
				"Options: 1) Try adding delays between actions, 2) Access different pages first, " +
				"3) Some sites have very aggressive bot detection that may be unavoidable."
		}
		if data.Partial {
			payload["partial"] = true
		}
		content, err := wrapEnvelope(NavigateName, payload)
		if err != nil {
			completed("error", "error", data.ElementCount, len([]rune(data.Snapshot)), len([]rune(rawURL)))
			return governance.Result{}, err
		}
		completed("ok", "ok", data.ElementCount, len([]rune(data.Snapshot)), len([]rune(rawURL)))
		result := governance.Result{Content: content, Outcome: governance.OutcomeProductive}
		if data.Partial {
			result.IsDegraded = true
			result.ReasonCode = "partial_load"
		}
		return result, nil
	}
}

func snapshotHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.snapshot", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, elementCount, snapshotChars int) {
			agentLog.Debug("agent.tool.browser.snapshot.complete", "browser snapshot completed",
				config.F("tool_name", SnapshotName), config.F("status", status), config.F("outcome", outcome),
				config.F("element_count", elementCount), config.F("snapshot_chars", snapshotChars),
				config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args, "full"); err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		full, err := boolArg(args, "full", false)
		if err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		agentLog.Debug("agent.tool.browser.snapshot.start", "starting browser snapshot", config.F("tool_name", SnapshotName))
		data, err := deps.Backend.Snapshot(ctx, profile, full)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				completed("error", "error", 0, 0)
				return governance.Result{}, fmt.Errorf("browser snapshot canceled: %w", ctxErr)
			}
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		content, err := boundSnapshot(SnapshotName, data.Snapshot, data.ElementCount, data.Path)
		if err != nil {
			completed("error", "error", data.ElementCount, len([]rune(data.Snapshot)))
			return governance.Result{}, err
		}
		completed("ok", "ok", data.ElementCount, len([]rune(data.Snapshot)))
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func clickHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.click", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, elementCount, snapshotChars int) {
			agentLog.Debug("agent.tool.browser.click.complete", "browser click completed",
				config.F("tool_name", ClickName), config.F("status", status), config.F("outcome", outcome),
				config.F("element_count", elementCount), config.F("snapshot_chars", snapshotChars),
				config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args, "ref"); err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		ref, ok := stringArg(args, "ref")
		if !ok || strings.TrimSpace(ref) == "" {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, errors.New("ref must be a non-empty element reference")
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		agentLog.Debug("agent.tool.browser.click.start", "starting browser click", config.F("tool_name", ClickName))
		data, err := deps.Backend.Click(ctx, profile, ref)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				completed("error", "error", 0, 0)
				return governance.Result{}, fmt.Errorf("browser click canceled: %w", ctxErr)
			}
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		content, err := boundSnapshot(ClickName, data.Snapshot, data.ElementCount, data.Path)
		if err != nil {
			completed("error", "error", data.ElementCount, len([]rune(data.Snapshot)))
			return governance.Result{}, err
		}
		completed("ok", "ok", data.ElementCount, len([]rune(data.Snapshot)))
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func typeHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (result governance.Result, err error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.type", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, elementCount, snapshotChars int) {
			agentLog.Debug("agent.tool.browser.type.complete", "browser type completed",
				config.F("tool_name", TypeName), config.F("status", status), config.F("outcome", outcome),
				config.F("element_count", elementCount), config.F("snapshot_chars", snapshotChars),
				config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args, "ref", "text"); err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		ref, ok := stringArg(args, "ref")
		if !ok || strings.TrimSpace(ref) == "" {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, errors.New("ref must be a non-empty element reference")
		}
		text, ok := stringArg(args, "text")
		if !ok {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, errors.New("text must be a string")
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		agentLog.Debug("agent.tool.browser.type.start", "starting browser type",
			config.F("tool_name", TypeName), config.F("text_chars", len([]rune(text))))
		data, err := deps.Backend.Fill(ctx, profile, ref, text)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				completed("error", "error", 0, 0)
				return governance.Result{}, fmt.Errorf("browser type canceled: %w", ctxErr)
			}
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		content, err := boundSnapshot(TypeName, data.Snapshot, data.ElementCount, data.Path)
		if err != nil {
			completed("error", "error", data.ElementCount, len([]rune(data.Snapshot)))
			return governance.Result{}, err
		}
		completed("ok", "ok", data.ElementCount, len([]rune(data.Snapshot)))
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func scrollHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.scroll", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, elementCount, snapshotChars int) {
			agentLog.Debug("agent.tool.browser.scroll.complete", "browser scroll completed",
				config.F("tool_name", ScrollName), config.F("status", status), config.F("outcome", outcome),
				config.F("element_count", elementCount), config.F("snapshot_chars", snapshotChars),
				config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args, "direction"); err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		direction, ok := stringArg(args, "direction")
		if !ok || (direction != "up" && direction != "down") {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, errors.New("direction must be up or down")
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		agentLog.Debug("agent.tool.browser.scroll.start", "starting browser scroll", config.F("tool_name", ScrollName))
		data, err := deps.Backend.Scroll(ctx, profile, direction)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				completed("error", "error", 0, 0)
				return governance.Result{}, fmt.Errorf("browser scroll canceled: %w", ctxErr)
			}
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		content, err := boundSnapshot(ScrollName, data.Snapshot, data.ElementCount, data.Path)
		if err != nil {
			completed("error", "error", data.ElementCount, len([]rune(data.Snapshot)))
			return governance.Result{}, err
		}
		completed("ok", "ok", data.ElementCount, len([]rune(data.Snapshot)))
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func backHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.back", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, elementCount, snapshotChars int) {
			agentLog.Debug("agent.tool.browser.back.complete", "browser back completed",
				config.F("tool_name", BackName), config.F("status", status), config.F("outcome", outcome),
				config.F("element_count", elementCount), config.F("snapshot_chars", snapshotChars),
				config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args); err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		agentLog.Debug("agent.tool.browser.back.start", "starting browser back", config.F("tool_name", BackName))
		data, err := deps.Backend.Back(ctx, profile)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				completed("error", "error", 0, 0)
				return governance.Result{}, fmt.Errorf("browser back canceled: %w", ctxErr)
			}
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		payload := map[string]interface{}{
			"url": data.URL, "title": data.Title, "snapshot": data.Snapshot, "element_count": data.ElementCount,
		}
		if data.Partial {
			payload["partial"] = true
		}
		content, err := wrapEnvelope(BackName, payload)
		if err != nil {
			completed("error", "error", data.ElementCount, len([]rune(data.Snapshot)))
			return governance.Result{}, err
		}
		completed("ok", "ok", data.ElementCount, len([]rune(data.Snapshot)))
		result := governance.Result{Content: content, Outcome: governance.OutcomeProductive}
		if data.Partial {
			result.IsDegraded = true
			result.ReasonCode = "partial_load"
		}
		return result, nil
	}
}

func pressHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.press", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, elementCount, snapshotChars int) {
			agentLog.Debug("agent.tool.browser.press.complete", "browser press completed",
				config.F("tool_name", PressName), config.F("status", status), config.F("outcome", outcome),
				config.F("element_count", elementCount), config.F("snapshot_chars", snapshotChars),
				config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args, "key"); err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		key, ok := stringArg(args, "key")
		if !ok || strings.TrimSpace(key) == "" {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, errors.New("key must be a non-empty key name")
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		agentLog.Debug("agent.tool.browser.press.start", "starting browser press", config.F("tool_name", PressName))
		data, err := deps.Backend.Press(ctx, profile, key)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				completed("error", "error", 0, 0)
				return governance.Result{}, fmt.Errorf("browser press canceled: %w", ctxErr)
			}
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		content, err := boundSnapshot(PressName, data.Snapshot, data.ElementCount, data.Path)
		if err != nil {
			completed("error", "error", data.ElementCount, len([]rune(data.Snapshot)))
			return governance.Result{}, err
		}
		completed("ok", "ok", data.ElementCount, len([]rune(data.Snapshot)))
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func getImagesHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.get_images", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, imageCount int) {
			agentLog.Debug("agent.tool.browser.get_images.complete", "browser get images completed",
				config.F("tool_name", GetImagesName), config.F("status", status), config.F("outcome", outcome),
				config.F("image_count", imageCount), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args); err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		agentLog.Debug("agent.tool.browser.get_images.start", "starting browser get images", config.F("tool_name", GetImagesName))
		data, err := deps.Backend.Images(ctx, profile)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				completed("error", "error", 0)
				return governance.Result{}, fmt.Errorf("browser images canceled: %w", ctxErr)
			}
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		images := make([]map[string]interface{}, 0, len(data.Images))
		for _, img := range data.Images {
			entry := map[string]interface{}{"src": img.Src}
			if img.Alt != "" {
				entry["alt"] = img.Alt
			}
			if img.Width > 0 {
				entry["width"] = img.Width
			}
			if img.Height > 0 {
				entry["height"] = img.Height
			}
			images = append(images, entry)
		}
		content, err := wrapEnvelope(GetImagesName, map[string]interface{}{"images": images, "count": data.Count})
		if err != nil {
			completed("error", "error", data.Count)
			return governance.Result{}, err
		}
		outcome := governance.OutcomeProductive
		reason := ""
		if data.Count == 0 {
			outcome = governance.OutcomeUnproductive
			reason = "no_results"
		}
		completed("ok", string(outcome), data.Count)
		return governance.Result{Content: content, Outcome: outcome, ReasonCode: reason}, nil
	}
}

func visionHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.vision", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, imageBytes int) {
			agentLog.Debug("agent.tool.browser.vision.complete", "browser vision completed",
				config.F("tool_name", VisionName), config.F("status", status), config.F("outcome", outcome),
				config.F("image_bytes", imageBytes), config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		principalCheck, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principalCheck.Authenticated() || principalCheck.Gateway == "openai" {
			completed("rejected", "rejected", 0)
			return governance.Result{}, errors.New("browser vision requires an authenticated image-capable request")
		}
		if err := rejectUnknownArgs(args, "question", "annotate"); err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		question, ok := stringArg(args, "question")
		question = strings.TrimSpace(question)
		if !ok || question == "" || !utf8.ValidString(question) || len([]rune(question)) > 2000 {
			completed("rejected", "rejected", 0)
			return governance.Result{}, errors.New("question must contain 1 to 2000 characters")
		}
		annotate, err := boolArg(args, "annotate", false)
		if err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0)
			return governance.Result{}, err
		}
		agentLog.Debug("agent.tool.browser.vision.start", "starting browser vision",
			config.F("tool_name", VisionName), config.F("question_chars", len([]rune(question))))
		shot, err := deps.Backend.Screenshot(ctx, profile, annotate)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				completed("error", "error", 0)
				return governance.Result{}, fmt.Errorf("browser vision canceled: %w", ctxErr)
			}
			completed("error", "error", 0)
			return governance.Result{}, err
		}
		if deps.Cache == nil {
			completed("error", "error", len(shot.PNG))
			return governance.Result{}, errors.New("browser vision cache is unavailable")
		}
		path, err := deps.Cache.Save(ctx, profile, shot.PNG, "image/png")
		if err != nil {
			completed("error", "error", len(shot.PNG))
			return governance.Result{}, errors.New("browser vision image could not be stored")
		}
		normalized, err := media.NormalizeVisionImage(ctx, shot.PNG, "image/png", nil)
		if err != nil {
			completed("error", "error", len(shot.PNG))
			return governance.Result{}, err
		}
		geometry := &llm.ImageGeometry{SourceWidth: normalized.OriginalWidth, SourceHeight: normalized.OriginalHeight, Width: normalized.Width, Height: normalized.Height}
		normalized.Image.Geometry = geometry
		content := "Screenshot captured into your context — you can see it natively now.\nUse your built-in vision to answer the user.\n\nQuestion: " + question + "\n\nscreenshot_path: " + path
		content += media.ImageGeometryNote(geometry)
		completed("ok", "ok", len(shot.PNG))
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive, Images: []llm.InputImage{normalized.Image}}, nil
	}
}

func consoleHandler(deps Deps) registry.Handler {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		started := time.Now()
		meta := requestctx.MetadataFromContext(ctx)
		principal, _ := requestctx.PrincipalFromContext(ctx)
		agentLog := deps.Log.Agent("agent.tool.browser.console", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...)
		completed := func(status, outcome string, messageCount, errorCount int) {
			agentLog.Debug("agent.tool.browser.console.complete", "browser console completed",
				config.F("tool_name", ConsoleName), config.F("status", status), config.F("outcome", outcome),
				config.F("message_count", messageCount), config.F("error_count", errorCount),
				config.F("duration_ms", time.Since(started).Milliseconds()))
		}
		if err := ctx.Err(); err != nil {
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		if err := rejectUnknownArgs(args, "clear", "expression"); err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		profile, err := profileFromContext(ctx)
		if err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		if raw, exists := args["expression"]; exists {
			expression, ok := raw.(string)
			if !ok || strings.TrimSpace(expression) == "" {
				completed("rejected", "rejected", 0, 0)
				return governance.Result{}, errors.New("expression must be a non-empty string")
			}
			agentLog.Debug("agent.tool.browser.console.start", "starting browser eval", config.F("tool_name", ConsoleName))
			eval, err := deps.Backend.Eval(ctx, profile, expression)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					completed("error", "error", 0, 0)
					return governance.Result{}, fmt.Errorf("browser eval canceled: %w", ctxErr)
				}
				completed("error", "error", 0, 0)
				return governance.Result{}, err
			}
			content, err := wrapEnvelope(ConsoleName, map[string]interface{}{"result": eval.Result})
			if err != nil {
				completed("error", "error", 0, 0)
				return governance.Result{}, err
			}
			completed("ok", "ok", 0, 0)
			return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
		}
		clear, err := boolArg(args, "clear", false)
		if err != nil {
			completed("rejected", "rejected", 0, 0)
			return governance.Result{}, err
		}
		agentLog.Debug("agent.tool.browser.console.start", "starting browser console", config.F("tool_name", ConsoleName))
		data, err := deps.Backend.Console(ctx, profile, clear)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				completed("error", "error", 0, 0)
				return governance.Result{}, fmt.Errorf("browser console canceled: %w", ctxErr)
			}
			completed("error", "error", 0, 0)
			return governance.Result{}, err
		}
		messages := make([]map[string]interface{}, 0, len(data.Messages))
		for _, m := range data.Messages {
			entry := map[string]interface{}{"type": m.Type, "text": m.Text}
			if m.Source != "" {
				entry["source"] = m.Source
			}
			messages = append(messages, entry)
		}
		jsErrors := make([]map[string]interface{}, 0, len(data.Errors))
		for _, e := range data.Errors {
			entry := map[string]interface{}{"message": e.Message}
			if e.Source != "" {
				entry["source"] = e.Source
			}
			jsErrors = append(jsErrors, entry)
		}
		content, err := wrapEnvelope(ConsoleName, map[string]interface{}{
			"console_messages": messages, "js_errors": jsErrors,
			"total_messages": data.TotalMessages, "total_errors": data.TotalErrors,
		})
		if err != nil {
			completed("error", "error", data.TotalMessages, data.TotalErrors)
			return governance.Result{}, err
		}
		completed("ok", "ok", data.TotalMessages, data.TotalErrors)
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}
