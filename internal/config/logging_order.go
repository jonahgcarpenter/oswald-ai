package config

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

var logHeaderOrder = []string{
	"ts", "level", "event", "component", "user_id", "gateway", "request_id",
	"operation_id", "parent_operation_id", "job_id", "workload", "model",
	"service", "log_type", "instance_id", "record_kind", "msg",
}

var logOutcomeOrder = []string{
	"status", "outcome", "execution_outcome", "delivery_outcome", "persistence_status",
	"execution_status", "delivery_status", "reason_code",
}

// marshalOrderedLog encodes already-filtered scalars without relying on map
// iteration order. Headers lead; operation fields sort by key; outcomes,
// timings, and classified errors follow. Values retain JSON escaping.
func marshalOrderedLog(payload map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(payload))
	used := make(map[string]bool, len(payload))
	appendKnown := func(order []string) {
		for _, key := range order {
			if _, ok := payload[key]; ok {
				keys = append(keys, key)
				used[key] = true
			}
		}
	}
	appendKnown(logHeaderOrder)
	var operation, timing []string
	for key := range payload {
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
	appendKnown(logOutcomeOrder)
	sort.Strings(timing)
	keys = append(keys, timing...)
	appendKnown([]string{"error_code"})
	var output bytes.Buffer
	output.WriteByte('{')
	for i, key := range keys {
		value, err := json.Marshal(payload[key])
		if err != nil {
			return nil, err
		}
		if i > 0 {
			output.WriteByte(',')
		}
		encodedKey, _ := json.Marshal(key)
		output.Write(encodedKey)
		output.WriteByte(':')
		output.Write(value)
	}
	output.WriteByte('}')
	return output.Bytes(), nil
}
