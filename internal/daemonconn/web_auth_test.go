package daemonconn

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebSignInManagementUsesMasterDaemonConnection(t *testing.T) {
	t.Parallel()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "synthetic-master", r.Header.Get("X-Api-Key"))
		assert.Empty(t, r.Header.Get("Cookie"))
		if r.Method == http.MethodGet {
			assert.Equal(t, "/api/daemon/web-auth/sessions", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"id":"` + id + `","created_at":"2026-01-01T00:00:00Z","expires_at":"2026-01-02T00:00:00Z"}]}`))
			return
		}
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/api/daemon/web-auth/sessions/"+id, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	c := New(ts.URL, "synthetic-master")
	records, err := c.WebSignIns(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, id, records[0].ID)
	require.NoError(t, c.RevokeWebSignIn(t.Context(), id))
	require.Error(t, c.RevokeWebSignIn(t.Context(), "../escape"))
}
