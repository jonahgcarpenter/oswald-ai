package websearch

import (
	"encoding/json"
	"errors"
	"math"
)

const (
	// DefaultWebResults is the web_search result limit when omitted.
	DefaultWebResults = 5
	// MaxWebResults is the largest permitted web_search result limit.
	MaxWebResults = 100
)

// WebResultLimit validates the optional web_search limit argument.
func WebResultLimit(args map[string]interface{}) (int, error) {
	raw, exists := args["limit"]
	if !exists {
		return DefaultWebResults, nil
	}
	const message = "limit must be an integer between 1 and 100"
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
			return 0, errors.New(message)
		}
	default:
		return 0, errors.New(message)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value < 1 || value > float64(MaxWebResults) {
		return 0, errors.New(message)
	}
	return int(value), nil
}
