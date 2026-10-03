package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/startup"
)

// run loads the selected configuration root and runs the long-lived service
// until the context is cancelled or startup fails. It never calls os.Exit; the
// caller maps the result to an exit code.
func run(ctx context.Context, root string, stdout io.Writer) int {
	cfg, err := config.LoadProfiles(root)
	// The banner is cosmetic and only renders on a terminal.
	if file, ok := stdout.(*os.File); ok {
		startup.PrintBanner(file)
	}
	if err != nil {
		fatalConfig(config.NewLogger(config.LevelInfo).Server("app"), "app.config.invalid", "invalid runtime configuration", err)
		return exitError
	}
	rootLog := config.NewLogger(cfg.LogLevel)
	log := rootLog.Server("app")
	log.Debug("app.config.loaded", "loaded runtime configuration", config.F("log_level", cfg.LogLevel.String()))
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	err = startup.Run(ctx, cfg, rootLog, stdout)
	stop()
	if err != nil {
		var startupErr *startup.Error
		if errors.As(err, &startupErr) {
			fatalConfig(log, startupErr.Event, startupErr.Message, startupErr.Cause)
		} else {
			fatalConfig(log, "app.start.failed", "application startup failed", err)
		}
		return exitError
	}
	return exitOK
}

// fatalConfig logs a fixed event and safe configuration diagnostics: the
// classification code, schema path, artifact label, and referenced variable
// name, never any configuration value.
func fatalConfig(log *config.Logger, event, message string, err error) {
	fields := []config.Field{config.ErrorField(err)}
	if configErr, ok := config.AsConfigError(err); ok {
		fields = append(fields, configErr.LogFields()...)
	}
	log.Error(event, message, fields...)
}
