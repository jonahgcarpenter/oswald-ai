package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"
)

const serviceName = "oswald-ai"

// Level represents a logging severity level.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String returns the lowercase label for a Level.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
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

// Field is a structured log field.
type Field struct {
	Key   string
	Value any
}

// F creates a structured log field.
func F(key string, value any) Field {
	return Field{Key: key, Value: value}
}

// ErrorField creates the standard error field when err is non-nil.
func ErrorField(err error) Field {
	if err == nil {
		return Field{}
	}
	return F("error", err.Error())
}

// Logger emits structured JSON logs to stderr.
type Logger struct {
	level  Level
	logger *log.Logger
	fields []Field
}

// NewLogger creates a Logger that writes JSON to stderr at the given minimum level.
func NewLogger(level Level) *Logger {
	return &Logger{
		level:  level,
		logger: log.New(os.Stderr, "", 0),
		fields: []Field{F("service", serviceName)},
	}
}

// With returns a logger that always includes the supplied fields.
func (l *Logger) With(fields ...Field) *Logger {
	merged := make([]Field, 0, len(l.fields)+len(fields))
	merged = append(merged, l.fields...)
	for _, field := range fields {
		if field.Key == "" {
			continue
		}
		merged = append(merged, field)
	}
	return &Logger{level: l.level, logger: l.logger, fields: merged}
}

// Server returns a server-scoped logger for the given component.
func (l *Logger) Server(component string, fields ...Field) *Logger {
	base := []Field{F("log_type", "server"), F("component", component)}
	base = append(base, fields...)
	return l.With(base...)
}

// Agent returns an agent-scoped logger with the full agent foundation attached.
func (l *Logger) Agent(component, requestID, sessionID, userID, gateway, model string, fields ...Field) *Logger {
	base := []Field{
		F("log_type", "agent"),
		F("component", component),
		F("request_id", requestID),
		F("session_id", sessionID),
		F("user_id", userID),
		F("gateway", gateway),
		F("model", model),
	}
	base = append(base, fields...)
	return l.With(base...)
}

func (l *Logger) log(level Level, event, msg string, fields ...Field) {
	if level < l.level {
		return
	}

	payload := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"level": level.String(),
		"event": event,
		"msg":   msg,
	}

	for _, field := range l.fields {
		if field.Key == "" || field.Value == nil {
			continue
		}
		payload[field.Key] = field.Value
	}
	for _, field := range fields {
		if field.Key == "" || field.Value == nil {
			continue
		}
		payload[field.Key] = field.Value
	}

	line, err := json.Marshal(payload)
	if err != nil {
		fallback := map[string]any{
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"level":   "error",
			"service": serviceName,
			"event":   "logger.marshal_failed",
			"msg":     "failed to marshal log payload",
			"error":   err.Error(),
		}
		line, _ = json.Marshal(fallback)
	}

	l.logger.Print(string(line))
}

// Debug logs a message at DEBUG level.
func (l *Logger) Debug(event, msg string, fields ...Field) {
	l.log(LevelDebug, event, msg, fields...)
}

// Info logs a message at INFO level.
func (l *Logger) Info(event, msg string, fields ...Field) {
	l.log(LevelInfo, event, msg, fields...)
}

// Warn logs a message at WARN level.
func (l *Logger) Warn(event, msg string, fields ...Field) {
	l.log(LevelWarn, event, msg, fields...)
}

// Error logs a message at ERROR level.
func (l *Logger) Error(event, msg string, fields ...Field) {
	l.log(LevelError, event, msg, fields...)
}

// Fatal logs a message at ERROR level then terminates the process.
func (l *Logger) Fatal(event, msg string, fields ...Field) {
	l.log(LevelError, event, msg, fields...)
	os.Exit(1)
}

// NewRequestID creates a short per-request correlation ID.
func NewRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "req_unknown"
	}
	return "req_" + hex.EncodeToString(b)
}
