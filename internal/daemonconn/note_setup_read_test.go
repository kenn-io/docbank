package daemonconn

import (
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonauth"
)

func TestNoteLocalTextPlanUsesProvenConnection(t *testing.T) {
	t.Parallel()
	const key = "synthetic-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == daemonauth.KeyChallengePath {
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			if err != nil {
				http.Error(w, "invalid nonce", http.StatusBadRequest)
				return
			}
			_ = json.MarshalWrite(w, map[string]string{"proof": daemonauth.KeyProof(key, nonce)})
			return
		}
		assert.Equal(t, "/api/v1/notes/local-text-plan", r.URL.Path)
		assert.Equal(t, key, r.Header.Get("X-Api-Key"))
		_ = json.MarshalWrite(w, api.NoteLocalTextPlan{})
	}))
	t.Cleanup(server.Close)
	conn, err := NewPushConnection(server.URL, key)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
}
