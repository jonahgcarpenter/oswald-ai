package websearch

import (
	"encoding/json"
	"fmt"
	"math"
)

const (
	// DefaultWebResults is the web.search result limit when omitted.
	DefaultWebResults = 5
	// MaxWebResults is the largest permitted web.search result limit.
	MaxWebResults = 8
	// DefaultImageResults is the web.image_search result limit when omitted.
	DefaultImageResults = 2
	// MaxImageResults is the largest permitted web.image_search result limit.
	MaxImageResults = 4
)

// ResultLimit validates the optional results argument. Only omission selects the
// default; explicit values must be whole numbers in the inclusive range 1..maxResults.
func ResultLimit(args map[string]interface{}, defaultResults, maxResults int) (int, error) {
	raw, exists := args["results"]
	if !exists {
		return defaultResults, nil
	}
	var value float64
	switch number := raw.(type) {
	case float64:
		value = number
	case int:
		value = float64(number)
	case int64:
		value = float64(number)
	case json.Number:
		var err error
		value, err = number.Float64()
		if err != nil {
			return 0, fmt.Errorf("results must be an integer between 1 and %d", maxResults)
		}
	default:
		return 0, fmt.Errorf("results must be an integer between 1 and %d", maxResults)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value < 1 || value > float64(maxResults) {
		return 0, fmt.Errorf("results must be an integer between 1 and %d", maxResults)
	}
	return int(value), nil
}
