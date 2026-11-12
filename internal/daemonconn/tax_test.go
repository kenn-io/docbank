package daemonconn

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

const taxAccountFixture = `{"account_id":"11111111-1111-4111-8111-111111111111","filer_party_id":"22222222-2222-4222-8222-222222222222","country":"ZZ","tax_kind":"Example","cadence":"annual","currency":"USD","created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","revision":1}`

func TestTaxClientRequestContracts(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/api/v1/tax/accounts/"+id, r.URL.Path)
		assert.Equal(t, `"2"`, r.Header.Get("If-Match"))
		var body map[string]any
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		assert.Contains(t, body, "effective_start")
		assert.Nil(t, body["effective_start"])
		assert.Contains(t, body, "note")
		assert.Empty(t, body["note"])
		assert.Contains(t, body, "lead_days")
		assert.InDelta(t, 0, body["lead_days"], 0)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"3"`)
		_, _ = w.Write([]byte(strings.Replace(taxAccountFixture, `"revision":1`, `"revision":3`, 1)))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").UpdateTaxAccount(t.Context(), id, 2, api.PatchTaxAccountRequest{ClearEffectiveStart: true, Note: new(""), LeadDays: new(int64(0))})
	require.NoError(t, err)
}

func TestTaxClientPageAndPrivateErrors(t *testing.T) {
	const sentinel = "private-synthetic-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, sentinel, r.URL.Query().Get("taxkind"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"account_id":"invalid","revision":1}],"limit":100,"offset":0}`))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").TaxAccounts(t.Context(), "", "", sentinel, false, 100, 0)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sentinel)
	failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, sentinel, http.StatusInternalServerError) }))
	t.Cleanup(failure.Close)
	_, err = New(failure.URL, "").TaxAccounts(t.Context(), "", "", sentinel, false, 100, 0)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sentinel)
}

func TestTaxClientListAndRevealQueries(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/tax/accounts" {
			assert.Equal(t, id, r.URL.Query().Get("filer"))
			assert.Equal(t, "true", r.URL.Query().Get("include_retired"))
			assert.Equal(t, "example", r.URL.Query().Get("taxkind"))
			assert.Equal(t, "ZZ", r.URL.Query().Get("country"))
			assert.Equal(t, "10", r.URL.Query().Get("limit"))
			assert.Equal(t, "2", r.URL.Query().Get("offset"))
			_, _ = w.Write([]byte(`{"items":[],"limit":10,"offset":2}`))
			return
		}
		assert.Equal(t, "true", r.URL.Query().Get("reveal"))
		w.Header().Set("ETag", `"1"`)
		_, _ = w.Write([]byte(taxAccountFixture))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	page, err := c.TaxAccounts(t.Context(), id, "ZZ", "example", true, 10, 2)
	require.NoError(t, err)
	assert.Empty(t, page.Items)
	_, err = c.TaxAccount(t.Context(), id, true)
	require.NoError(t, err)
}

func TestTaxClientRejectsMalformedAccounts(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct{ name, body, etag string }{
		{"etag mismatch", taxAccountFixture, `"2"`},
		{"missing identity", `{"revision":1}`, `"1"`},
		{"different identity", strings.Replace(taxAccountFixture, id, "33333333-3333-4333-8333-333333333333", 1), `"1"`},
		{"zero revision", strings.Replace(taxAccountFixture, `"revision":1`, `"revision":0`, 1), `"0"`},
		{"missing etag", taxAccountFixture, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.etag != "" {
					w.Header().Set("ETag", tc.etag)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			_, err := New(server.URL, "").TaxAccount(t.Context(), id, false)
			require.Error(t, err)
			assert.True(t, IsResponseDecodeError(err))
		})
	}
}

func TestTaxClientValidationDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"status":422,"code":"invalid_tax","detail":"cadence: unknown cadence"}`))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").CreateTaxAccount(t.Context(), api.CreateTaxAccountRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cadence: unknown cadence")
	code, ok := ProblemCode(err)
	assert.True(t, ok)
	assert.Equal(t, "invalid_tax", code)
	untrusted := safeTaxError(apiProblemError(api.Error{Status: http.StatusUnprocessableEntity, Code: "invalid_tax", Detail: "cadence: unknown cadence private-synthetic-value"}))
	assert.Contains(t, untrusted.Error(), "cadence: unknown cadence")
	assert.NotContains(t, untrusted.Error(), "private-synthetic-value")
	audit := safeTaxError(apiProblemError(api.Error{Status: http.StatusConflict, Code: "audit_mutation_unsupported", Detail: "private-synthetic-value"}))
	assert.Contains(t, audit.Error(), "tax writes are unavailable in audited vaults")
	assert.NotContains(t, audit.Error(), "private-synthetic-value")
}

func TestTaxClientRevisionExhaustion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":409,"code":"tax_revision_exhausted","detail":"private-synthetic-value"}`))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").RetireTaxAccount(t.Context(), "11111111-1111-4111-8111-111111111111", 1)
	require.ErrorIs(t, err, store.ErrTaxRevisionExhausted)
	status, ok := ResponseStatus(err)
	assert.True(t, ok)
	assert.Equal(t, http.StatusConflict, status)
	code, ok := ProblemCode(err)
	assert.True(t, ok)
	assert.Equal(t, "tax_revision_exhausted", code)
	assert.Contains(t, err.Error(), code)
	assert.NotContains(t, err.Error(), "private-synthetic-value")
}

func TestTaxClientIncompleteReceiptsAndUnknownOutcome(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	var creates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/tax/accounts" {
			creates++
			w.WriteHeader(http.StatusCreated)
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/tax/accounts" {
			_, _ = w.Write([]byte(`{"items":[{"account_id":"` + id + `","revision":1}],"limit":100,"offset":0}`))
			return
		}
		_, _ = w.Write([]byte(`{"account_id":"` + id + `","revision":1}`))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	_, err := c.TaxAccount(t.Context(), id, false)
	require.True(t, IsResponseDecodeError(err))
	_, err = c.TaxAccounts(t.Context(), "", "", "", false, 100, 0)
	require.True(t, IsResponseDecodeError(err))
	_, err = c.CreateTaxAccount(t.Context(), api.CreateTaxAccountRequest{})
	require.True(t, IsResponseDecodeError(err))
	assert.Contains(t, err.Error(), "may have been applied")
	assert.Contains(t, err.Error(), "inspect")
	assert.Equal(t, 1, creates)
	_, err = c.UpdateTaxAccount(t.Context(), id, 1, api.PatchTaxAccountRequest{Note: new("example")})
	require.True(t, IsResponseDecodeError(err))
	assert.Contains(t, err.Error(), "may have been applied")
	_, err = c.RetireTaxAccount(t.Context(), id, 1)
	require.True(t, IsResponseDecodeError(err))
	assert.Contains(t, err.Error(), "may have been applied")
}

func TestTaxClientRetiredCause(t *testing.T) {
	err := safeTaxError(apiProblemError(api.Error{Status: http.StatusConflict, Code: "tax_retired", Detail: "private-synthetic-value"}))
	require.ErrorIs(t, err, store.ErrTaxRetired)
	assert.Contains(t, err.Error(), "tax_retired")
	assert.NotContains(t, err.Error(), "private-synthetic-value")
}

func TestTaxClientTransportErrorClass(t *testing.T) {
	err := taxMutationFailure(&transportError{err: errors.Join(context.DeadlineExceeded, errors.New("private-synthetic-value"))})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, IsTransportError(err))
	assert.Contains(t, err.Error(), "deadline exceeded")
	assert.Contains(t, err.Error(), "may have been applied")
	assert.NotContains(t, err.Error(), "private-synthetic-value")
}
