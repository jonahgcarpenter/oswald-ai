package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/startup"
)

func main() {
	cfg, err := config.Load()
	startup.PrintBanner(os.Stdout)
	if err != nil {
		fatalConfig(config.NewLogger(config.LevelInfo).Server("app"), "app.config.invalid", "invalid runtime configuration", err)
	}
	rootLog := config.NewLogger(cfg.LogLevel)
	log := rootLog.Server("app")
	log.Debug("app.config.loaded", "loaded runtime configuration", config.F("log_level", cfg.LogLevel.String()))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = startup.Run(ctx, cfg, rootLog, os.Stdout)
	stop()
	if err != nil {
		var startupErr *startup.Error
		if errors.As(err, &startupErr) {
			fatalConfig(log, startupErr.Event, startupErr.Message, startupErr.Cause)
		}
		fatalConfig(log, "app.start.failed", "application startup failed", err)
	}
}

// fatalConfig logs a fixed event and message with safe configuration
// diagnostics: the classification code, schema path, artifact label, and
// referenced variable name, never any configuration value.
func fatalConfig(log *config.Logger, event, message string, err error) {
	fields := []config.Field{config.ErrorField(err)}
	if configErr, ok := config.AsConfigError(err); ok {
		fields = append(fields, configErr.LogFields()...)
	}
	log.Fatal(event, message, fields...)
}
