// Package currenttime implements the time.current builtin tool.
package currenttime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

type result struct {
	Timezone     string `json:"timezone"`
	Datetime     string `json:"datetime"`
	UTCDateTime  string `json:"utc_datetime"`
	UTCOffset    string `json:"utc_offset"`
	Abbreviation string `json:"abbreviation"`
	Weekday      string `json:"weekday"`
}

// NewHandler returns a handler that reports the current time in a requested timezone.
func NewHandler(now func() time.Time) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(_ context.Context, args map[string]interface{}) (governance.Result, error) {
		rawTimezone, ok := args["timezone"]
		if !ok {
			return governance.Result{}, fmt.Errorf("time.current: timezone is required")
		}
		timezone, ok := rawTimezone.(string)
		if !ok {
			return governance.Result{}, fmt.Errorf("time.current: timezone must be a string")
		}
		timezone = strings.TrimSpace(timezone)
		if timezone == "" {
			return governance.Result{}, fmt.Errorf("time.current: timezone is required")
		}
		if timezone == "Local" {
			return governance.Result{}, fmt.Errorf("time.current: timezone %q is host-dependent; use an IANA timezone or UTC", timezone)
		}

		location, err := time.LoadLocation(timezone)
		if err != nil {
			return governance.Result{}, fmt.Errorf("time.current: invalid timezone %q: %w", timezone, err)
		}

		currentUTC := now().UTC()
		currentLocal := currentUTC.In(location)
		abbreviation, _ := currentLocal.Zone()
		encoded, err := json.Marshal(result{
			Timezone:     timezone,
			Datetime:     currentLocal.Format(time.RFC3339),
			UTCDateTime:  currentUTC.Format(time.RFC3339),
			UTCOffset:    currentLocal.Format("-07:00"),
			Abbreviation: abbreviation,
			Weekday:      currentLocal.Weekday().String(),
		})
		if err != nil {
			return governance.Result{}, fmt.Errorf("time.current: encode result: %w", err)
		}
		return governance.Result{Content: string(encoded), Outcome: governance.OutcomeProductive}, nil
	}
}
