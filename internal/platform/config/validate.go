package config

// Validate runs the exact startup pipeline (parse, env merge, interpolation,
// per-section checks) against a candidate configuration root on disk without
// touching live state: no permission repair and no repair logging. It returns
// the effective config on success so CLI, reload, and startup share
// diagnostics through ConfigError (code + schema path + variable name, never
// values).
func Validate(root string) (*Config, error) { return loadProfiles(root, false) }
