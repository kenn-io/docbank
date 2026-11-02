package embedclient

import (
	"errors"
	"net/http"
	"time"

	"go.kenn.io/kit/embedconfig"
)

// ErrOrigin reports that a request left the configured endpoint origin.
var ErrOrigin = errors.New("embed request origin does not match the configured endpoint")

type originTransport struct {
	base   http.RoundTripper
	origin string
	apiKey string
}

func (t *originTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	origin, err := embedconfig.Origin(req.URL)
	if err != nil || origin != t.origin {
		return nil, ErrOrigin
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.apiKey == "" || req.Header.Get("Authorization") != "" {
		return base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.apiKey)
	return base.RoundTrip(clone)
}

func pinClient(caller *http.Client, origin, apiKey string, timeout time.Duration) *http.Client {
	if caller == nil {
		caller = &http.Client{Timeout: timeout}
	}
	clone := *caller
	clone.Transport = &originTransport{base: caller.Transport, origin: origin, apiKey: apiKey}
	return &clone
}
