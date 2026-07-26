package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

const serviceName = "oswald-ai"

var reservedLogFields = map[string]struct{}{
	"ts": {}, "level": {}, "service": {}, "log_type": {}, "component": {}, "event": {}, "msg": {},
}

var validLogStatuses = map[string]struct{}{
	"ok": {}, "error": {}, "rejected": {}, "retry": {}, "degraded": {},
}

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
	return F("error", SafeErrorText(err))
}

// Logger emits structured JSON logs to stderr.
type Logger struct {
	level     Level
	logger    *log.Logger
	logType   string
	component string
	fields    []Field
	agent     []Field
}

// NewLogger creates a Logger that writes JSON to stderr at the given minimum level.
func NewLogger(level Level) *Logger {
	return &Logger{
		level:     level,
		logger:    log.New(os.Stderr, "", 0),
		logType:   "server",
		component: "app",
	}
}

// With returns a logger that always includes the supplied fields.
func (l *Logger) With(fields ...Field) *Logger {
	merged := make([]Field, 0, len(l.fields)+len(fields))
	merged = append(merged, l.fields...)
	for _, field := range fields {
		if field.Key == "" || isReservedLogField(field.Key) {
			continue
		}
		merged = append(merged, field)
	}
	return &Logger{level: l.level, logger: l.logger, logType: l.logType, component: l.component, fields: merged, agent: l.agent}
}

// SetOutput changes the destination used by this logger and its scoped children.
func (l *Logger) SetOutput(w io.Writer) {
	l.logger.SetOutput(w)
}

// Server returns a server-scoped logger for the given component.
func (l *Logger) Server(component string, fields ...Field) *Logger {
	scoped := l.With(fields...)
	scoped.logType = "server"
	scoped.component = component
	return scoped
}

// Agent returns an agent-scoped logger with the full agent foundation attached.
func (l *Logger) Agent(component, requestID, sessionID, userID, gateway, model string, fields ...Field) *Logger {
	scoped := l.With(fields...)
	scoped.logType = "agent"
	scoped.component = component
	scoped.agent = []Field{
		F("request_id", requestID),
		F("session_id", sessionID),
		F("user_id", userID),
		F("gateway", gateway),
		F("model", model),
	}
	return scoped
}

func (l *Logger) log(level Level, event, msg string, fields ...Field) {
	if level < l.level {
		return
	}

	payload := map[string]any{
		"ts":        time.Now().UTC().Format(time.RFC3339Nano),
		"level":     level.String(),
		"service":   serviceName,
		"log_type":  l.logType,
		"component": l.component,
		"event":     event,
		"msg":       msg,
	}

	for _, field := range l.fields {
		if field.Key == "" || field.Value == nil || isReservedLogField(field.Key) {
			continue
		}
		addLogField(payload, field)
	}
	for _, field := range fields {
		if field.Key == "" || field.Value == nil || isReservedLogField(field.Key) {
			continue
		}
		addLogField(payload, field)
	}
	for _, field := range l.agent {
		payload[field.Key] = field.Value
	}

	line, err := json.Marshal(payload)
	if err != nil {
		fallback := map[string]any{
			"ts":        time.Now().UTC().Format(time.RFC3339Nano),
			"level":     "error",
			"service":   serviceName,
			"log_type":  l.logType,
			"component": l.component,
			"event":     "logger.marshal_failed",
			"msg":       "failed to marshal log payload",
			"status":    "error",
			"error":     SafeErrorText(err),
		}
		for _, field := range l.agent {
			fallback[field.Key] = field.Value
		}
		line, _ = json.Marshal(fallback)
	}

	l.logger.Print(string(line))
}

func isReservedLogField(key string) bool {
	_, reserved := reservedLogFields[key]
	return reserved
}

func addLogField(payload map[string]any, field Field) {
	if field.Key == "status" {
		status, ok := field.Value.(string)
		if !ok {
			payload[field.Key] = "degraded"
			return
		}
		if _, valid := validLogStatuses[status]; !valid {
			payload[field.Key] = "degraded"
			return
		}
	}
	payload[field.Key] = field.Value
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
