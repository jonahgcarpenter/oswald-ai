package config

import (
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// modelSelectionValueRuneLimit bounds each model-selection leaf, matching the
// persisted conversation-pin field bound.
const modelSelectionValueRuneLimit = 256

// SetModelSelection writes a profile's model.provider and model.default in
// ONE mediated commit: the candidate document is staged through the exact
// startup validation pipeline, the previous bytes are kept as last-known-good,
// and a single atomic replace lands both keys together. Nothing touches disk
// when validation fails. provider is the bare provider name; the selector
// prefix custom: is written by this function so callers cannot forget it.
func SetModelSelection(root, profile, provider, model string) error {
	if err := validateModelSelectionValue(provider, "model.provider"); err != nil {
		return err
	}
	if err := validateModelSelectionValue(model, "model.default"); err != nil {
		return err
	}
	configPath, err := ConfigPathForProfile(root, profile)
	if err != nil {
		return err
	}
	orig, node, err := readManagedDocRaw(configPath, profile)
	if err != nil {
		return err
	}
	if err := assignManaged(node, []string{"model", "provider"}, managedString("custom:"+provider)); err != nil {
		return err
	}
	if err := assignManaged(node, []string{"model", "default"}, managedString(model)); err != nil {
		return err
	}
	candidate, err := marshalManaged(node)
	if err != nil {
		return err
	}
	return CommitConfigCandidate(root, configPath, candidate, orig)
}

// validateModelSelectionValue rejects empty, oversized, or invalid UTF-8
// values with a classified error that never carries the value.
func validateModelSelectionValue(value, path string) error {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > modelSelectionValueRuneLimit {
		return configErr("config_value_invalid", path)
	}
	return nil
}

// managedString always produces a string scalar so numeric-looking model IDs
// keep their string type through the managed document round trip.
func managedString(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
