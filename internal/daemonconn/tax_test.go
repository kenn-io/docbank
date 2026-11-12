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

func TestTaxClientRequestContracts(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/api/v1/tax/accounts/"+id, r.URL.Path)
		assert.Equal(t, `"2"`, r.Header.Get("If-Match"))
		var body map[string]any
		require.NoError(t, json.UnmarshalRead(r.Body, &body))
		assert.Contains(t, body, "effective_start")
		assert.Nil(t, body["effective_start"])
		assert.Equal(t, "", body["note"])
		assert.Equal(t, float64(0), body["lead_days"])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"3"`)
		_, _ = w.Write([]byte(`{"account_id":"` + id + `","revision":3}`))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").UpdateTaxAccount(t.Context(), id, 2, api.PatchTaxAccountRequest{ClearEffectiveStart: true, Note: new(""), LeadDays: new(int64(0))})
	require.NoError(t, err)
}

func TestTaxClientResponseIdentityAndETag(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"2"`)
		_, _ = w.Write([]byte(`{"account_id":"` + id + `","revision":1}`))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").TaxAccount(t.Context(), id, false)
	require.Error(t, err)
}

func TestTaxClientPageAndPrivateErrors(t *testing.T) {
	const sentinel = "private-synthetic-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, sentinel, r.URL.Query().Get("taxkind"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"account_id":"invalid","revision":1}],"limit":100,"offset":0}`))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").TaxAccounts(t.Context(), api.TaxAccountListOptions{TaxKind: sentinel, Limit: 100})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sentinel)
}

func TestTaxClientListAndRevealQueries(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/tax/accounts" {
			assert.Equal(t, "example", r.URL.Query().Get("taxkind"))
			assert.Equal(t, "ZZ", r.URL.Query().Get("country"))
			assert.Equal(t, "10", r.URL.Query().Get("limit"))
			assert.Equal(t, "2", r.URL.Query().Get("offset"))
			_, _ = w.Write([]byte(`{"items":[],"limit":10,"offset":2}`))
			return
		}
		assert.Equal(t, "true", r.URL.Query().Get("reveal"))
		w.Header().Set("ETag", `"1"`)
		_, _ = w.Write([]byte(`{"account_id":"` + id + `","revision":1}`))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	page, err := c.TaxAccounts(t.Context(), api.TaxAccountListOptions{Country: "ZZ", TaxKind: "example", Limit: 10, Offset: 2})
	require.NoError(t, err)
	assert.Empty(t, page.Items)
	_, err = c.TaxAccount(t.Context(), id, true)
	require.NoError(t, err)
}
