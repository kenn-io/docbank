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

func TestContractClientEditSendsExplicitEmptyValues(t *testing.T) {
	t.Parallel()
	const id = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/api/v1/contracts/"+id, r.URL.Path)
		assert.Equal(t, `"2"`, r.Header.Get("If-Match"))
		var body map[string]any
		require.NoError(t, json.UnmarshalRead(r.Body, &body))
		assert.Equal(t, map[string]any{
			"title": "", "contract_type": "", "status": "", "starts_on": "", "expires_on": nil,
			"summary": "", "purpose": "", "consideration": "", "termination": "",
		}, body)
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	_, err := c.UpdateContract(t.Context(), id, 2, api.PatchContractRequest{
		Title: new(""), ContractType: new(""), Status: new(""), StartsOn: new(""), ClearExpires: true,
		Summary: new(""), Purpose: new(""), Consideration: new(""), Termination: new(""),
	})
	require.Error(t, err)
}
