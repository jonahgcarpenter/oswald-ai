package config

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// logEnvelopeOrder is the fixed universal envelope. Every record carries these
// keys (details last); event-specific data lives inside details.
var logEnvelopeOrder = []string{
	"ts", "level", "event", "component", "log_type", "details",
}

// detailsCorrelationOrder leads the nested details object, keeping canonical
// correlation in a stable position ahead of event-specific fields.
var detailsCorrelationOrder = []string{
	"msg", "profile", "user_id", "gateway", "request_id",
	"operation_id", "parent_operation_id", "job_id", "workload", "model",
}

var logOutcomeOrder = []string{
	"status", "outcome", "execution_outcome", "delivery_outcome", "persistence_status",
	"execution_status", "delivery_status", "reason_code",
}

// marshalOrderedLog encodes the already-filtered envelope without relying on map
// iteration order. The envelope leads; details is emitted last as a nested
// object with deterministic inner ordering.
func marshalOrderedLog(payload map[string]any) ([]byte, error) {
	var output bytes.Buffer
	output.WriteByte('{')
	first := true
	writeKey := func(key string) {
		if !first {
			output.WriteByte(',')
		}
		first = false
		encodedKey, _ := json.Marshal(key)
		output.Write(encodedKey)
		output.WriteByte(':')
	}
	for _, key := range logEnvelopeOrder {
		if key == "details" {
			continue
		}
		value, ok := payload[key]
		if !ok {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		writeKey(key)
		output.Write(encoded)
	}
	if detailsValue, ok := payload["details"]; ok {
		writeKey("details")
		if details, ok := detailsValue.(map[string]any); ok {
			encoded, err := marshalDetails(details)
			if err != nil {
				return nil, err
			}
			output.Write(encoded)
		} else {
			encoded, err := json.Marshal(detailsValue)
			if err != nil {
				return nil, err
			}
			output.Write(encoded)
		}
	}
	// Defensive: encode any unexpected top-level keys deterministically.
	var extra []string
	for key := range payload {
		if key == "details" {
			continue
		}
		known := false
		for _, ordered := range logEnvelopeOrder {
			if ordered == key {
				known = true
				break
			}
		}
		if !known {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		encoded, err := json.Marshal(payload[key])
		if err != nil {
			return nil, err
		}
		writeKey(key)
		output.Write(encoded)
	}
	output.WriteByte('}')
	return output.Bytes(), nil
}

func marshalDetails(details map[string]any) ([]byte, error) {
	keys := orderedDetailKeys(details)
	var output bytes.Buffer
	output.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			output.WriteByte(',')
		}
		encodedKey, _ := json.Marshal(key)
		output.Write(encodedKey)
		output.WriteByte(':')
		encoded, err := json.Marshal(details[key])
		if err != nil {
			return nil, err
		}
		output.Write(encoded)
	}
	output.WriteByte('}')
	return output.Bytes(), nil
}

func orderedDetailKeys(details map[string]any) []string {
	used := make(map[string]bool, len(details))
	keys := make([]string, 0, len(details))
	for _, key := range detailsCorrelationOrder {
		if _, ok := details[key]; ok {
			keys = append(keys, key)
			used[key] = true
		}
	}
	var operation, timing []string
	for key := range details {
		if used[key] || key == "error_code" {
			continue
		}
		isOutcome := false
		for _, known := range logOutcomeOrder {
			isOutcome = isOutcome || key == known
		}
		if isOutcome {
			continue
		}
		if strings.HasSuffix(key, "_ms") {
			timing = append(timing, key)
		} else {
			operation = append(operation, key)
		}
	}
	sort.Strings(operation)
	keys = append(keys, operation...)
	for _, key := range logOutcomeOrder {
		if _, ok := details[key]; ok {
			keys = append(keys, key)
			used[key] = true
		}
	}
	sort.Strings(timing)
	keys = append(keys, timing...)
	if _, ok := details["error_code"]; ok {
		keys = append(keys, "error_code")
	}
	return keys
}
