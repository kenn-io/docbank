package daemonconn

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/docbank/internal/api"
)

var ErrAgentSessionOrigin = errors.New("agent session request left its daemon origin")

// AttachAgentSession constructs a client for one already-running loopback
// daemon. It neither discovers nor restarts a daemon, and it never holds the
// master key. The transport applies the session credential to every request.
func AttachAgentSession(baseURL, token string) (*Connection, error) {
	origin, err := url.Parse(baseURL)
	if err != nil || origin.Scheme != "http" || origin.Host == "" || origin.User != nil ||
		(origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.Fragment != "" ||
		!agentSessionLoopback(origin.Hostname()) || len(token) == 0 || len(token) > 512 ||
		strings.ContainsAny(token, "\r\n\x00") {
		return nil, errors.New("agent session requires one loopback daemon origin and token")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("unsupported daemon HTTP transport")
	}
	next := base.Clone()
	next.Proxy = nil
	connection := New(strings.TrimRight(baseURL, "/"), "")
	connection.hc = &http.Client{
		Transport: &agentSessionTransport{next: next, origin: origin, token: token},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return connection, nil
}

func agentSessionLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type agentSessionTransport struct {
	next   http.RoundTripper
	origin *url.URL
	token  string
}

func (t *agentSessionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL == nil || !strings.EqualFold(request.URL.Scheme, t.origin.Scheme) ||
		!strings.EqualFold(request.URL.Host, t.origin.Host) || request.URL.User != nil ||
		request.URL.Opaque != "" ||
		(request.Host != "" && !strings.EqualFold(request.Host, t.origin.Host)) {
		return nil, ErrAgentSessionOrigin
	}
	request = request.Clone(request.Context())
	request.Header = request.Header.Clone()
	request.Header.Del("X-Api-Key")
	request.Header.Del("Authorization")
	request.Header.Del(api.WebSessionHeader)
	request.Header.Del("Cookie")
	request.Header.Del(api.AgentSessionHeader)
	request.Header.Set(api.AgentSessionHeader, t.token)
	return t.next.RoundTrip(request)
}

func (t *agentSessionTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
