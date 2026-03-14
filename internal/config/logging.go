package config

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// Level represents a logging severity level.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String returns the uppercase label for a Level.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// ParseLevel converts a string (case-insensitive) to a Level.
// Unknown values default to INFO.
func ParseLevel(s string) Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return LevelDebug
	case "WARN", "WARNING":
		return LevelWarn
	case "ERROR":
		return LevelError
	default:
		return LevelInfo
	}
}

// Logger is a leveled logger backed by the stdlib log package.
// It gates output by the configured minimum Level.
type Logger struct {
	level  Level
	logger *log.Logger
}

// NewLogger creates a Logger that writes to stderr at the given minimum level.
func NewLogger(level Level) *Logger {
	return &Logger{
		level:  level,
		logger: log.New(os.Stderr, "", log.LstdFlags),
	}
}

func (l *Logger) log(level Level, format string, args ...any) {
	if level < l.level {
		return
	}
	prefix := fmt.Sprintf("[%s] ", level)
	l.logger.Output(3, prefix+fmt.Sprintf(format, args...)) // nolint: errcheck
}

// Debug logs a message at DEBUG level.
func (l *Logger) Debug(format string, args ...any) {
	l.log(LevelDebug, format, args...)
}

// Info logs a message at INFO level.
func (l *Logger) Info(format string, args ...any) {
	l.log(LevelInfo, format, args...)
}

// Warn logs a message at WARN level.
func (l *Logger) Warn(format string, args ...any) {
	l.log(LevelWarn, format, args...)
}

// Error logs a message at ERROR level.
func (l *Logger) Error(format string, args ...any) {
	l.log(LevelError, format, args...)
}

// Fatal logs a message at ERROR level then terminates the process.
func (l *Logger) Fatal(format string, args ...any) {
	l.log(LevelError, format, args...)
	os.Exit(1)
}
