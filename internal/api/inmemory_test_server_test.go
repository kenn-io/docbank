package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

// newInMemoryTestServer builds the same server as newTestServer but serves it
// over httptest's in-memory network. Call it inside a synctest bubble: a real
// loopback listener blocks in network I/O, which keeps synctest.Wait from
// returning, while the in-memory network blocks durably between requests.
func newInMemoryTestServer(t *testing.T, mutate func(*api.Deps)) (*httptest.Server, *testStore) {
	t.Helper()
	loopback, live := newTestServer(t, mutate)
	loopback.Close()
	handler := live.Server.Handler()
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The in-memory network uses documentation addresses; present the
		// request as the loopback client newTestServer's callers expect.
		r.RemoteAddr = "127.0.0.1:1234"
		r.Host = "127.0.0.1"
		handler.ServeHTTP(w, r)
	}))
	client := server.Client()
	base, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	client.Transport = inMemoryClientTransport{
		apiKeyTransport: &apiKeyTransport{key: testAPIKey, next: base}, base: base,
	}
	return server, live
}

// inMemoryClientTransport keeps CloseIdleConnections reachable so the test
// server's Close can release the client's pooled connections.
type inMemoryClientTransport struct {
	*apiKeyTransport

	base *http.Transport
}

func (t inMemoryClientTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }
