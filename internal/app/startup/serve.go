package startup

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// ServerPIDFile is the PID marker the service writes inside the configuration
// root so `oswald restart` can signal the running process in place.
const ServerPIDFile = "server.pid"

// Serve loads one configuration root and runs the service until the context is
// cancelled or startup fails. It logs classified diagnostics itself and returns
// the cause after cleanup; it never calls os.Exit. The terminal banner is only
// rendered when stdout is a terminal *os.File.
//
// The server writes <root>/server.pid (0600) on boot and removes it on clean
// shutdown. `oswald restart` sends SIGTERM to that PID; the container runtime
// restarts the container in the same pod. Load-bearing config and plugin
// changes never restart automatically: mutating CLI commands print
// "run `oswald restart` to apply" and non-hot watcher hits only log
// `config.reload.restart_required` as an advisory.
func Serve(ctx context.Context, root string, stdout io.Writer) error {
	cfg, err := config.LoadProfiles(root)
	if file, ok := stdout.(*os.File); ok {
		PrintBanner(file)
	}
	if err != nil {
		logFailure(config.NewLogger(config.LevelInfo).Server("app"), "app.config.invalid", "invalid runtime configuration", err)
		return err
	}
	rootLog := config.NewLogger(cfg.LogLevel)
	log := rootLog.Server("app")
	log.Debug("app.config.loaded", "loaded runtime configuration", config.F("log_level", cfg.LogLevel.String()))
	writeServerPID(cfg.ProfileRoot, log)
	defer removeServerPID(cfg.ProfileRoot)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	err = Run(ctx, cfg, rootLog, stdout)
	stop()
	if err != nil {
		var startupErr *Error
		if errors.As(err, &startupErr) {
			logFailure(log, startupErr.Event, startupErr.Message, startupErr.Cause)
		} else {
			logFailure(log, "app.start.failed", "application startup failed", err)
		}
		return err
	}
	return nil
}

// writeServerPID records the serving process ID for `oswald restart`.
// Failures are advisory: PID 1 containers and read-only roots must still serve.
func writeServerPID(root string, log *config.Logger) {
	path := filepath.Join(root, ServerPIDFile)
	if err := config.AtomicWritePrivateFile(path, []byte(strconv.Itoa(os.Getpid()))); err != nil {
		log.Warn("app.pid.write_failed", "could not write server pid file", config.ErrorField(err))
	}
}

// removeServerPID clears the PID marker on clean shutdown. A stale file after
// a crash is tolerated: `oswald restart` treats a dead PID as not running and
// removes it.
func removeServerPID(root string) {
	_ = os.Remove(filepath.Join(root, ServerPIDFile))
}

// logFailure logs a fixed event with safe configuration diagnostics: the
// classification code, schema path, artifact label, and referenced variable
// name, never any configuration value.
func logFailure(log *config.Logger, event, message string, err error) {
	fields := []config.Field{config.ErrorField(err)}
	if configErr, ok := config.AsConfigError(err); ok {
		fields = append(fields, configErr.LogFields()...)
	}
	log.Error(event, message, fields...)
}
