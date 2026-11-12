package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPartyClientEditSendsRevisionAndClear(t *testing.T) {
	t.Parallel()
	const id = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/api/v1/parties/"+id, r.URL.Path)
		assert.Equal(t, `"2"`, r.Header.Get("If-Match"))
		var body map[string]any
		require.NoError(t, json.UnmarshalRead(r.Body, &body))
		assert.Contains(t, body, "country")
		assert.Nil(t, body["country"])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"3"`)
		_, _ = w.Write([]byte(`{"party_id":"` + id + `","kind":"organization","display_name":"Example","legal_name":"","country":null,"person_id":null,"own":false,"revision":3,"created_at":"2026-01-01T00:00:00.000000000Z","updated_at":"2026-01-01T00:00:00.000000000Z","retired_at":null}`))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	party, err := c.UpdateParty(t.Context(), id, 2, api.PatchPartyRequest{ClearCountry: true})
	require.NoError(t, err)
	assert.Equal(t, int64(3), party.Revision)
}
