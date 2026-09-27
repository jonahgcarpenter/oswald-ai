package websearch

import (
	"encoding/json"
	"math"
	"testing"
)

func TestWebResultLimit(t *testing.T) {
	if got, err := WebResultLimit(nil); err != nil || got != DefaultWebResults {
		t.Fatalf("omitted: got %d, err %v", got, err)
	}
	if got, err := WebResultLimit(map[string]interface{}{"results": 2}); err != nil || got != DefaultWebResults {
		t.Fatalf("unrelated argument changed text default: got %d, err %v", got, err)
	}
	for _, raw := range []interface{}{1, int64(1), float64(1), json.Number("1"), json.Number("1.0"), float64(MaxWebResults)} {
		got, err := WebResultLimit(map[string]interface{}{"limit": raw})
		if err != nil || got < 1 || got > MaxWebResults {
			t.Errorf("valid %v (%T): got %d, err %v", raw, raw, got, err)
		}
	}
	for _, raw := range []interface{}{nil, "2", true, 0, -1, MaxWebResults + 1, 1.5, math.NaN(), math.Inf(1), math.Inf(-1), json.Number("bad"), json.Number("1.5"), json.Number("1e1000"), []interface{}{1}} {
		if _, err := WebResultLimit(map[string]interface{}{"limit": raw}); err == nil {
			t.Errorf("accepted invalid %v (%T)", raw, raw)
		}
	}
}
