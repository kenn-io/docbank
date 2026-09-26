package daemonconn

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestAttachedAgentSessionStripsBroadCredentialsAndFencesOrigin(t *testing.T) {
	var observed http.Header
	var foreignVisits int
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignVisits++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(foreign.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = r.Header.Clone()
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, foreign.URL+"/leak", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	connection, err := AttachAgentSession(server.URL, "synthetic-agent-token")
	require.NoError(t, err)
	require.Empty(t, connection.key)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/read", nil)
	require.NoError(t, err)
	request.Header.Set("X-Api-Key", "broad-key")
	request.Header.Set("Authorization", "Bearer broad-key")
	request.Header.Set(api.WebSessionHeader, "browser-token")
	response, err := connection.hc.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	require.Equal(t, "synthetic-agent-token", observed.Get(api.AgentSessionHeader))
	require.Empty(t, observed.Get("X-Api-Key"))
	require.Empty(t, observed.Get("Authorization"))
	require.Empty(t, observed.Get(api.WebSessionHeader))

	redirect, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/redirect", nil)
	require.NoError(t, err)
	response, err = connection.hc.Do(redirect)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusFound, response.StatusCode)
	require.Zero(t, foreignVisits)

	away, err := http.NewRequestWithContext(t.Context(), http.MethodGet, foreign.URL+"/leak", nil)
	require.NoError(t, err)
	response, err = connection.hc.Do(away)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorIs(t, err, ErrAgentSessionOrigin)
	require.Zero(t, foreignVisits)

	spoofed, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/read", nil)
	require.NoError(t, err)
	spoofed.Host = "foreign.example.test"
	response, err = connection.hc.Do(spoofed)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorIs(t, err, ErrAgentSessionOrigin)
}
