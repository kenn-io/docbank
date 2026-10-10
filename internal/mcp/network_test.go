package mcp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetworkMCPGuardPreservesHostOriginAndBearerBoundary(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler, err := wrapHTTPTransport(inner, HTTPOptions{BearerToken: testMCPBearer, listenHost: "0.0.0.0", AllowedHosts: []string{"docbank:7341"}})
	require.NoError(t, err)
	for _, test := range []struct {
		host, origin, token string
		status              int
	}{
		{"docbank:7341", "", testMCPBearer, 204},
		{"docbank:7341", "", "", 401}, {"docbank:7341", "", "wrong", 401},
		{"docbank:7341", "http://docbank:7341", testMCPBearer, 204},
		{"docbank:7341", "http://attacker.example", testMCPBearer, 403},
		{"docbank:7341", "http://docbank:7342", testMCPBearer, 403},
		{"docbank:7342", "", testMCPBearer, 403},
		{"attacker.example:7341", "", testMCPBearer, 403},
		{"0.0.0.0:7341", "", testMCPBearer, 403},
		{"127.0.0.1:7341", "", testMCPBearer, 204},
	} {
		t.Run(test.host+test.origin+test.token, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", nil)
			request.Host = test.host
			request.Header.Set("Authorization", "Bearer "+test.token)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			assert.Equal(t, test.status, recorder.Code)
		})
	}
	_, err = wrapHTTPTransport(inner, HTTPOptions{BearerToken: testMCPBearer, AllowedHosts: []string{"*"}})
	require.Error(t, err)
}

func TestAllowedNetworkHostPassesSDKOnLoopbackConnection(t *testing.T) {
	handler, err := newTestServer().HTTPTransportHandler(HTTPOptions{BearerToken: testMCPBearer, AllowedHosts: []string{"docbank:7341"}})
	require.NoError(t, err)
	listener := httptest.NewServer(handler)
	t.Cleanup(listener.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, listener.URL+"/mcp", bytes.NewReader(fixture(t, "discover.json")))
	require.NoError(t, err)
	request.Header = protocolHeaders("server/discover", "")
	request.Header.Set("Authorization", "Bearer "+testMCPBearer)
	request.Host = "docbank:7341"
	response, err := listener.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	assert.Equal(t, http.StatusOK, response.StatusCode)
}
