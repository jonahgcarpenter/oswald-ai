package mcp

import "net/http"

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	clone := req.Clone(req.Context())
	for name, value := range t.headers {
		if name == "" || value == "" {
			continue
		}
		clone.Header.Set(name, value)
	}
	return base.RoundTrip(clone)
}
