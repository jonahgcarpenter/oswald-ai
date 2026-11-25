package startup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/profiles"
)

// Hot-apply covers the shared log level, gateway admission policy (allow/ban
// lists, group mention rules), and live model routing: the default model,
// provider, endpoint, and key plus the selected routing overrides of profiles
// present both before and after, and the global platform channel-override map.
// The log level and admission policy swap on the next watcher tick; model
// routing is rotated into the running router views, summarizers, and delegate
// configs by the attached applier, while a channel-override-only diff swaps
// router views without re-seating summarizers. Model windows (context lengths)
// and profile additions/removals stay restart-class. Everything else
// (listeners, credentials, routes, mention patterns, tool backends, MCP
// sessions, plugin installs) is load-bearing and needs `oswald restart`:
// mutating CLI commands print "run `oswald restart` to apply" and the watcher
// only logs an advisory `config.reload.restart_required`. Restart is explicit
// so the model batches edits and restarts once, never killing its own turn
// with an automatic rebuild.
const (
	reloadPollInterval = 2 * time.Second
	reloadSettleDelay  = 500 * time.Millisecond
)

// Reloader watches profile configuration files and validation-gates every
// change: invalid candidates keep the current config live, hot-only diffs swap
// in place, and validated non-hot diffs only log an advisory
// `config.reload.restart_required` (the operator runs `oswald restart` when
// ready). Hot model-routing diffs also invoke the attached model applier,
// which rotates router views, re-seats summarizers, and swaps delegate live
// configs (a channel-override-only diff swaps views only). The Directory is
// the configuration truth after a successful swap;
// an applier failure only WARNs `config.reload.apply_failed` and keeps the
// previous runtime view, so runtime views can lag the Directory until the next
// tick or a restart. The reloader never rewrites operator files and never
// restarts automatically.
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
	applierMu sync.RWMutex
	applier   func(old, next *config.Config)
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

// SetModelApplier attaches the live model-routing rotate hook. It is safe to
// call after Start, including while the poll goroutine runs; a nil apply
// disables the hook. The applier runs only when a hot diff includes the
// `model`, `providers`, or `channel_overrides` class, and owns its own failure
// logging so a runtime view that could not rotate never fails the
// configuration swap.
func (r *Reloader) SetModelApplier(apply func(old, next *config.Config)) {
	if r == nil {
		return
	}
	r.applierMu.Lock()
	r.applier = apply
	r.applierMu.Unlock()
}

// modelApplier snapshots the attached hook under the lock.
func (r *Reloader) modelApplier() func(old, next *config.Config) {
	if r == nil {
		return nil
	}
	r.applierMu.RLock()
	defer r.applierMu.RUnlock()
	return r.applier
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
	oldSeen := r.seen
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
	hot, classes := config.ClassifyReload(r.last, candidate)
	if pluginPathsChanged(oldSeen, stable) {
		classes = appendPluginClass(classes)
		r.log.Debug("config.reload.restart_required", "configuration change needs `oswald restart`", config.F("trigger", "watcher"), config.F("reason_code", strings.Join(classes, ",")))
		return
	}
	if !hot {
		r.log.Debug("config.reload.restart_required", "configuration change needs `oswald restart`", config.F("trigger", "watcher"), config.F("reason_code", strings.Join(classes, ",")))
		return
	}
	if err := r.directory.Swap(candidate); err != nil {
		r.log.Warn("config.reload.rejected", "rejected configuration swap", config.ErrorField(err), config.F("trigger", "watcher"))
		return
	}
	r.rootLog.SetLevel(candidate.LogLevel)
	if apply := r.modelApplier(); apply != nil && (classListed(classes, "model") || classListed(classes, "providers") || classListed(classes, "channel_overrides")) {
		apply(r.last, candidate)
	}
	r.last = candidate
	r.log.Debug("config.reload.applied", "applied configuration reload", config.F("trigger", "watcher"))
}

// classListed reports whether a reload class is present in the classifier's
// differing-section list.
func classListed(classes []string, name string) bool {
	for _, class := range classes {
		if class == name {
			return true
		}
	}
	return false
}

// fingerprint records size and mtime for every config artifact, including
// absent markers so creation and deletion are changes too. It also covers
// per-profile plugin markers (manifest, install metadata, node sidecar) so
// `plugins install/update/remove` surfaces the same restart
// advisory without any automatic restart.
func fingerprint(root string) map[string]fileID {
	out := map[string]fileID{}
	paths := []string{filepath.Join(root, "config.yaml"), filepath.Join(root, ".env")}
	profileRoots := []string{root}
	entries, _ := os.ReadDir(filepath.Join(root, "profiles"))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		profileRoots = append(profileRoots, filepath.Join(root, "profiles", entry.Name()))
		paths = append(paths, filepath.Join(root, "profiles", entry.Name(), "config.yaml"), filepath.Join(root, "profiles", entry.Name(), ".env"))
	}
	for _, profileRoot := range profileRoots {
		paths = append(paths, pluginFingerprintPaths(profileRoot)...)
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

// pluginFingerprintPaths lists the small marker files that identify a
// per-profile plugin install. The node sidecar lives under code/node_modules
// (potentially thousands of files), so only the directory marker plus the
// manifest and install metadata are fingerprinted: enough to detect
// install/update/remove without walking the sidecar tree.
func pluginFingerprintPaths(profileRoot string) []string {
	pluginsRoot := filepath.Join(profileRoot, "plugins")
	entries, err := os.ReadDir(pluginsRoot)
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			// Hidden trees (including the generated .bin shim dir) are
			// derived state, never installs: keep them out of the
			// fingerprint so shim regeneration never retriggers itself.
			continue
		}
		if name == "plugins-backup" || (len(name) > 4 && name[:2] == "__" && name[len(name)-2:] == "__") {
			continue
		}
		codeDir := filepath.Join(pluginsRoot, name, "code")
		paths = append(paths,
			filepath.Join(codeDir, "plugin.yaml"),
			filepath.Join(codeDir, "plugin.yml"),
			filepath.Join(codeDir, ".install-metadata.json"),
			filepath.Join(codeDir, "node_modules"),
		)
	}
	return paths
}

// pluginPathsChanged reports whether any plugin marker path differs between
// two fingerprints. Config-only changes leave these identical. Absent markers
// compare by value, so a newly listed (still absent) path is not a change.
func pluginPathsChanged(old, next map[string]fileID) bool {
	for path, nextID := range next {
		if !isPluginMarkerPath(path) {
			continue
		}
		if oldID := old[path]; oldID != nextID {
			return true
		}
	}
	for path, oldID := range old {
		if !isPluginMarkerPath(path) {
			continue
		}
		if nextID := next[path]; oldID != nextID {
			return true
		}
	}
	return false
}

func isPluginMarkerPath(path string) bool {
	// Fingerprinted plugin markers always live under a plugins/ parent and
	// end in one of the small marker names (never inside node_modules).
	if !strings.Contains(path, string(filepath.Separator)+"plugins"+string(filepath.Separator)) {
		return false
	}
	base := filepath.Base(path)
	switch base {
	case "plugin.yaml", "plugin.yml", ".install-metadata.json", "node_modules":
		return true
	default:
		return false
	}
}

func appendPluginClass(classes []string) []string {
	for _, class := range classes {
		if class == "plugins" {
			return classes
		}
	}
	return append(classes, "plugins")
}

// classifyReload is kept for test compatibility; the canonical
// implementation lives in platform/config.ClassifyReload so CLI groups can
// report live-vs-restart effects without importing startup.
func classifyReload(old, next *config.Config) (bool, []string) {
	return config.ClassifyReload(old, next)
}
