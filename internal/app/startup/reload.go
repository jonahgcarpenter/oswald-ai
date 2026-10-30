package startup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/profiles"
)

// Hot-apply covers only state read live per message: the shared log level and
// gateway admission policy (allow/ban lists, group mention rules). Listeners,
// model clients, credentials, routes, mention patterns, tool backends, and MCP
// sessions are captured at assembly and need a restart.
const (
	reloadPollInterval = 2 * time.Second
	reloadSettleDelay  = 500 * time.Millisecond
)

// Reloader watches profile configuration files and validation-gates every
// change: invalid candidates keep the current config live, hot-only diffs swap
// in place, and anything else logs restart_required without touching live
// state. It never rewrites operator files.
type Reloader struct {
	root      string
	interval  time.Duration
	settle    time.Duration
	log       *config.Logger
	rootLog   *config.Logger
	directory *profiles.Directory
	last      *config.Config
	seen      map[string]fileID
	stop      chan struct{}
	done      chan struct{}
}

type fileID struct {
	size  int64
	mtime int64
	found bool
}

// NewReloader captures the current fingerprint and effective config. The
// directory and root log are updated in place on hot-only diffs.
func NewReloader(root string, last *config.Config, directory *profiles.Directory, rootLog *config.Logger) *Reloader {
	return &Reloader{
		root: root, interval: reloadPollInterval, settle: reloadSettleDelay,
		log:       rootLog.Server("app"),
		rootLog:   rootLog,
		directory: directory, last: last,
		seen: fingerprint(root),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// Start polls until the context ends or Stop runs.
func (r *Reloader) Start(ctx context.Context) {
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.stop:
				return
			case <-ticker.C:
				r.Check()
			}
		}
	}()
}

// Stop halts polling and joins the watcher.
func (r *Reloader) Stop() {
	if r == nil {
		return
	}
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
}

// Check runs one validate-and-apply pass. It is the same path the watcher and
// a manual reload share.
func (r *Reloader) Check() {
	if r == nil {
		return
	}
	next := fingerprint(r.root)
	if reflect.DeepEqual(next, r.seen) {
		return
	}
	time.Sleep(r.settle)
	stable := fingerprint(r.root)
	if !reflect.DeepEqual(next, stable) {
		r.seen = stable
		return
	}
	r.seen = stable
	candidate, err := config.Validate(r.root)
	if err != nil {
		fields := []config.Field{config.ErrorField(err), config.F("trigger", "watcher")}
		if cerr, ok := config.AsConfigError(err); ok {
			fields = append(fields, cerr.LogFields()...)
		}
		r.log.Warn("config.reload.rejected", "rejected invalid configuration candidate", fields...)
		return
	}
	hot, classes := classifyReload(r.last, candidate)
	if !hot {
		r.log.Debug("config.reload.restart_required", "configuration change needs a restart", config.F("trigger", "watcher"), config.F("reason_code", strings.Join(classes, ",")))
		return
	}
	if err := r.directory.Swap(candidate); err != nil {
		r.log.Warn("config.reload.rejected", "rejected configuration swap", config.ErrorField(err), config.F("trigger", "watcher"))
		return
	}
	r.rootLog.SetLevel(candidate.LogLevel)
	r.last = candidate
	r.log.Debug("config.reload.applied", "applied configuration reload", config.F("trigger", "watcher"))
}

// fingerprint records size and mtime for every config artifact, including
// absent markers so creation and deletion are changes too.
func fingerprint(root string) map[string]fileID {
	out := map[string]fileID{}
	paths := []string{filepath.Join(root, "config.yaml"), filepath.Join(root, ".env")}
	entries, _ := os.ReadDir(filepath.Join(root, "profiles"))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		paths = append(paths, filepath.Join(root, "profiles", entry.Name(), "config.yaml"), filepath.Join(root, "profiles", entry.Name(), ".env"))
	}
	sort.Strings(paths)
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			out[path] = fileID{}
			continue
		}
		out[path] = fileID{size: info.Size(), mtime: info.ModTime().UnixNano(), found: true}
	}
	return out
}

// classifyReload diffs the effective configs by section. Only the runtime log
// level and admission policy are hot; every other differing section forces a
// restart. The returned classes name the differing sections from a fixed
// vocabulary for logging.
func classifyReload(old, next *config.Config) (bool, []string) {
	if old == nil || next == nil {
		return false, []string{"unavailable"}
	}
	var classes []string
	add := func(name string, same bool) {
		if !same {
			classes = append(classes, name)
		}
	}
	add("runtime", old.LogLevel == next.LogLevel)
	add("admission", reflect.DeepEqual(old.DiscordPolicy, next.DiscordPolicy) &&
		reflect.DeepEqual(old.BlueBubblesPolicy, next.BlueBubblesPolicy) &&
		old.DiscordGroupRequireMention == next.DiscordGroupRequireMention &&
		old.BlueBubblesGroupRequireMention == next.BlueBubblesGroupRequireMention)
	add("providers", reflect.DeepEqual(old.LLMGatewayURL, next.LLMGatewayURL) &&
		reflect.DeepEqual(old.LLMGatewayAPIKey, next.LLMGatewayAPIKey) &&
		old.LLMGatewayProvider == next.LLMGatewayProvider)
	add("model", old.LLMGatewayModel == next.LLMGatewayModel && old.ModelContextWindow == next.ModelContextWindow)
	add("api", old.OpenAIAPIKey == next.OpenAIAPIKey && old.OpenAIListenHost == next.OpenAIListenHost && old.OpenAIListenPort == next.OpenAIListenPort)
	add("gateway", reflect.DeepEqual(old.ProfileRoutes, next.ProfileRoutes))
	add("platforms", old.DiscordToken == next.DiscordToken &&
		old.BlueBubblesURL == next.BlueBubblesURL && old.BlueBubblesPassword == next.BlueBubblesPassword &&
		old.BlueBubblesListenPort == next.BlueBubblesListenPort && old.BlueBubblesWebhookPath == next.BlueBubblesWebhookPath &&
		old.BlueBubblesWebhookCallbackHost == next.BlueBubblesWebhookCallbackHost &&
		reflect.DeepEqual(old.BlueBubblesMentionPatterns, next.BlueBubblesMentionPatterns))
	add("tools", old.BraveAPIKey == next.BraveAPIKey && old.SearxngURL == next.SearxngURL &&
		old.ComfyUIURL == next.ComfyUIURL && old.ComfyUIGenerationTimeout == next.ComfyUIGenerationTimeout &&
		old.BrowserLaunchCommand == next.BrowserLaunchCommand && old.BrowserEndpointURL == next.BrowserEndpointURL &&
		old.BrowserCommandTimeout == next.BrowserCommandTimeout && old.BrowserSnapshotThreshold == next.BrowserSnapshotThreshold &&
		old.BrowserInactivityTimeout == next.BrowserInactivityTimeout && old.BrowserRecordSessions == next.BrowserRecordSessions &&
		old.BrowserAllowPrivateURLs == next.BrowserAllowPrivateURLs && old.BrowserAllowInsecureURLs == next.BrowserAllowInsecureURLs &&
		old.BrowserRestrictEvaluate == next.BrowserRestrictEvaluate)
	add("mcp_servers", reflect.DeepEqual(old.MCPServers, next.MCPServers))
	add("profiles", reflect.DeepEqual(old.Profiles, next.Profiles))
	if len(classes) == 0 {
		return true, nil
	}
	for _, class := range classes {
		if class != "runtime" && class != "admission" {
			return false, classes
		}
	}
	return true, classes
}
