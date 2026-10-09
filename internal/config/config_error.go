package config

import (
	"errors"
	"regexp"
	"strings"
)

var configPathSegmentRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ConfigError classifies a configuration failure with a stable code and an
// operator-safe location. It never carries configuration values.
type ConfigError struct {
	// Code is a fixed, developer-owned classification.
	Code string
	// Path is a schema-shaped location such as providers.<name>.api.
	Path string
	// Variable is the referenced environment variable name, never its value.
	Variable string
	// Source names the configuration artifact when no schema path applies.
	Source string
}

// Error returns fixed text; the classification and location travel as fields.
func (e *ConfigError) Error() string {
	if e == nil || e.Code == "" {
		return "invalid configuration"
	}
	return "invalid configuration: " + e.Code
}

// AsConfigError exposes the classification for startup diagnostics.
func AsConfigError(err error) (*ConfigError, bool) {
	var target *ConfigError
	if !errors.As(err, &target) || target == nil || target.Code == "" {
		return nil, false
	}
	return target, true
}

// LogFields returns reviewed, validated diagnostic fields. Unvalidated paths or
// variable names are omitted rather than logged.
func (e *ConfigError) LogFields() []Field {
	if e == nil {
		return nil
	}
	fields := make([]Field, 0, 3)
	if source := safeConfigSource(e.Source); source != "" {
		fields = append(fields, F("config_source", source))
	}
	if path := safeConfigPath(e.Path); path != "" {
		fields = append(fields, F("config_path", path))
	}
	if e.Variable != "" && len(e.Variable) <= 256 && environmentName.MatchString(e.Variable) {
		fields = append(fields, F("config_variable", e.Variable))
	}
	return fields
}

// knownConfigSources bounds the artifact labels that may appear in logs.
var knownConfigSources = map[string]bool{
	"global_config": true, "global_env": true, "global_root": true,
	"profile_config": true, "profile_env": true, "profile_root": true,
}

func safeConfigSource(source string) string {
	if knownConfigSources[source] {
		return source
	}
	return ""
}

// safeConfigPath allows only bounded schema-shaped dotted paths.
func safeConfigPath(path string) string {
	if path == "" || len(path) > 256 {
		return ""
	}
	for _, segment := range strings.Split(path, ".") {
		if !configPathSegmentRE.MatchString(segment) {
			return ""
		}
	}
	return path
}

func configErr(code, path string) error { return &ConfigError{Code: code, Path: path} }

func configVarErr(code, variable, path string) error {
	return &ConfigError{Code: code, Variable: variable, Path: path}
}

// withConfigPath attaches a schema location to a classified error that does not
// already carry one. Unclassified errors pass through unchanged.
func withConfigPath(err error, path string) error {
	var target *ConfigError
	if errors.As(err, &target) && target != nil && target.Path == "" {
		copy := *target
		copy.Path = path
		return &copy
	}
	return err
}

// withConfigSource attaches an artifact label to a classified error.
func withConfigSource(err error, source string) error {
	var target *ConfigError
	if errors.As(err, &target) && target != nil && target.Source == "" {
		copy := *target
		copy.Source = source
		return &copy
	}
	return err
}
