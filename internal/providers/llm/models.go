package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// modelsListMaxBytes bounds one models listing response body. Real
	// providers report hundreds of entries; 2 MiB is generous headroom.
	modelsListMaxBytes = 2 << 20
	// modelsListMaxEntries silently bounds the parsed listing. Providers
	// report hundreds of models, never thousands.
	modelsListMaxEntries = 1024
)

var (
	errModelsListUnavailable = errors.New("models list unavailable")
	errModelsListOversize    = errors.New("models list response oversize")
	errModelsListMalformed   = errors.New("models list response malformed")
)

// ModelEntry is one model from a provider's models listing. Window is the
// reported context length; 0 when the provider omits it.
type ModelEntry struct {
	ID     string
	Window int
}

// ListModels fetches the provider's OpenAI-compatible model listing from
// GET {BaseURL}/v1/models with the client's bearer/virtual-key headers. It
// never logs and its errors never embed the endpoint, credentials, or raw
// provider bytes.
func (c *GatewayClient) ListModels(ctx context.Context) ([]ModelEntry, error) {
	endpoint := c.BaseURL + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errModelsListUnavailable
	}
	c.applyHeaders(req)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// net/http transport errors embed the request URL, so only the
		// caller-cancelable context cause is safe to surface.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errModelsListUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models list request failed (status %d)", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsListMaxBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errModelsListUnavailable
	}
	if len(body) > modelsListMaxBytes {
		return nil, errModelsListOversize
	}

	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errModelsListMalformed
	}

	entries := make([]ModelEntry, 0, len(payload.Data))
	for _, raw := range payload.Data {
		if len(entries) >= modelsListMaxEntries {
			break
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			continue
		}
		var id string
		if err := json.Unmarshal(object["id"], &id); err != nil {
			continue
		}
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		entries = append(entries, ModelEntry{ID: id, Window: contextLengthFromObject(object)})
	}
	return entries, nil
}

// contextLengthFromObject reads an optional positive context window. The
// field may sit directly on the entry, inside a per-entry metadata object
// that some OpenAI-compatible providers emit, or inside OpenCode's
// limit object ({limit:{context:N}}); unknown shapes yield 0.
func contextLengthFromObject(object map[string]json.RawMessage) int {
	if window := positiveInt(object["context_length"]); window > 0 {
		return window
	}
	if window := positiveInt(object["context"]); window > 0 {
		return window
	}
	for _, key := range []string{"top_provider", "limit", "limits", "metadata"} {
		raw, ok := object[key]
		if !ok {
			continue
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err != nil {
			continue
		}
		if window := positiveInt(nested["context_length"]); window > 0 {
			return window
		}
		if window := positiveInt(nested["context"]); window > 0 {
			return window
		}
	}
	return 0
}

func positiveInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var value int
	if err := json.Unmarshal(raw, &value); err == nil {
		if value <= 0 {
			return 0
		}
		return value
	}
	var floatValue float64
	if err := json.Unmarshal(raw, &floatValue); err != nil {
		return 0
	}
	if floatValue <= 0 || floatValue > float64(1<<31-1) {
		return 0
	}
	return int(floatValue)
}
