package websearch

import (
	"encoding/json"
	"math"
	"testing"
)

func TestResultLimit(t *testing.T) {
	for _, limits := range []struct{ defaultResults, maxResults int }{{DefaultWebResults, MaxWebResults}, {DefaultImageResults, MaxImageResults}} {
		if got, err := ResultLimit(nil, limits.defaultResults, limits.maxResults); err != nil || got != limits.defaultResults {
			t.Fatalf("omitted: got %d, err %v", got, err)
		}
		for _, raw := range []interface{}{1, int64(1), float64(1), json.Number("1"), json.Number("1.0"), float64(limits.maxResults)} {
			got, err := ResultLimit(map[string]interface{}{"results": raw}, limits.defaultResults, limits.maxResults)
			if err != nil || got < 1 || got > limits.maxResults {
				t.Errorf("valid %v (%T): got %d, err %v", raw, raw, got, err)
			}
		}
		for _, raw := range []interface{}{nil, "2", true, 0, -1, limits.maxResults + 1, 1.5, math.NaN(), math.Inf(1), math.Inf(-1), json.Number("bad"), json.Number("1.5"), json.Number("1e1000"), []interface{}{1}} {
			if _, err := ResultLimit(map[string]interface{}{"results": raw}, limits.defaultResults, limits.maxResults); err == nil {
				t.Errorf("accepted invalid %v (%T)", raw, raw)
			}
		}
	}
}
