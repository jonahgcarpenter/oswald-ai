package currenttime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNewHandlerReturnsCurrentTimeInRequestedTimezone(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		timezone string
		want     result
	}{
		{
			name:     "UTC",
			now:      time.Date(2026, time.July, 17, 18, 32, 18, 0, time.UTC),
			timezone: "UTC",
			want: result{
				Timezone:     "UTC",
				Datetime:     "2026-07-17T18:32:18Z",
				UTCDateTime:  "2026-07-17T18:32:18Z",
				UTCOffset:    "+00:00",
				Abbreviation: "UTC",
				Weekday:      "Friday",
			},
		},
		{
			name:     "New York daylight saving time",
			now:      time.Date(2026, time.July, 17, 18, 32, 18, 0, time.UTC),
			timezone: "America/New_York",
			want: result{
				Timezone:     "America/New_York",
				Datetime:     "2026-07-17T14:32:18-04:00",
				UTCDateTime:  "2026-07-17T18:32:18Z",
				UTCOffset:    "-04:00",
				Abbreviation: "EDT",
				Weekday:      "Friday",
			},
		},
		{
			name:     "New York standard time",
			now:      time.Date(2026, time.January, 17, 18, 32, 18, 0, time.UTC),
			timezone: "America/New_York",
			want: result{
				Timezone:     "America/New_York",
				Datetime:     "2026-01-17T13:32:18-05:00",
				UTCDateTime:  "2026-01-17T18:32:18Z",
				UTCOffset:    "-05:00",
				Abbreviation: "EST",
				Weekday:      "Saturday",
			},
		},
		{
			name:     "fractional offset crossing midnight",
			now:      time.Date(2026, time.July, 17, 18, 32, 18, 0, time.UTC),
			timezone: "Asia/Kathmandu",
			want: result{
				Timezone:     "Asia/Kathmandu",
				Datetime:     "2026-07-18T00:17:18+05:45",
				UTCDateTime:  "2026-07-17T18:32:18Z",
				UTCOffset:    "+05:45",
				Abbreviation: "+0545",
				Weekday:      "Saturday",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewHandler(func() time.Time { return tt.now })
			raw, err := handler(context.Background(), map[string]interface{}{"timezone": tt.timezone})
			if err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			var got result
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("decode result %q: %v", raw, err)
			}
			if got != tt.want {
				t.Fatalf("result = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNewHandlerRejectsInvalidTimezoneArguments(t *testing.T) {
	tests := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{name: "missing", args: map[string]interface{}{}, want: "timezone is required"},
		{name: "empty", args: map[string]interface{}{"timezone": "  "}, want: "timezone is required"},
		{name: "wrong type", args: map[string]interface{}{"timezone": 123}, want: "timezone must be a string"},
		{name: "host local", args: map[string]interface{}{"timezone": "Local"}, want: "host-dependent"},
		{name: "unknown", args: map[string]interface{}{"timezone": "Mars/Olympus_Mons"}, want: "invalid timezone"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewHandler(time.Now)
			if _, err := handler(context.Background(), tt.args); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("handler error = %v, want error containing %q", err, tt.want)
			}
		})
	}
}
