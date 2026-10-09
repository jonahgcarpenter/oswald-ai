package startup

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Serve loads one configuration root and runs the service until the context is
// cancelled or startup fails. It logs classified diagnostics itself and returns
// the cause after cleanup; it never calls os.Exit. The terminal banner is only
// rendered when stdout is a terminal *os.File.
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
