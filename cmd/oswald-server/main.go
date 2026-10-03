// Command oswald-server runs the Oswald AI service. It owns configuration
// loading, the terminal banner, signal handling, and process exit; all runtime
// composition happens in internal/startup.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/startup"
)

func main() {
	fs := flag.NewFlagSet("oswald-server", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		_, _ = io.WriteString(os.Stderr, "usage: oswald-server [--root DIR]\n")
	}
	root := fs.String("root", config.OswaldHomeDir, "configuration root directory")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}
	os.Exit(run(*root))
}

// run loads the selected configuration root and runs the service until the
// context is cancelled or startup fails. It returns a process exit code.
func run(root string) int {
	cfg, err := config.LoadProfiles(root)
	// The banner is cosmetic and only renders on a terminal.
	startup.PrintBanner(os.Stdout)
	if err != nil {
		fatalConfig(config.NewLogger(config.LevelInfo).Server("app"), "app.config.invalid", "invalid runtime configuration", err)
		return 1
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
		} else {
			fatalConfig(log, "app.start.failed", "application startup failed", err)
		}
		return 1
	}
	return 0
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
