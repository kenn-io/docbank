package daemonconn

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

const taxRecordFixture = `{"record_id":"11111111-1111-4111-8111-111111111111","filer_party_id":"22222222-2222-4222-8222-222222222222","kind":"filing","title":"Example","country":"ZZ","tax_kind":"Example","state":"expected","direction":"unknown","verification":"unverified","revision":1,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z"}`

func TestTaxRecordClientContracts(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/tax/records" {
			q := r.URL.Query()
			assert.Equal(t, "2026", q.Get("year"))
			assert.Equal(t, "filing", q.Get("kind"))
			assert.Equal(t, "expected", q.Get("state"))
			assert.Equal(t, "zz", q.Get("country"))
			_, _ = w.Write([]byte(`{"items":[` + taxRecordFixture + `],"limit":1,"offset":0}`))
			return
		}
		assert.Equal(t, "/api/v1/tax/records/"+id, r.URL.Path)
		assert.Equal(t, `"2"`, r.Header.Get("If-Match"))
		var body map[string]any
		require.NoError(t, json.UnmarshalRead(r.Body, &body))
		for _, field := range []string{"authority_party_id", "tax_year", "period_start", "due_date", "lead_days", "amount"} {
			require.Contains(t, body, field)
			assert.Nil(t, body[field])
		}
		_, _ = w.Write([]byte(taxRecordFixture))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	page, err := c.TaxRecords(t.Context(), "", "zz", "", new(int64(2026)), "expected", "filing", false, 1, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	_, err = c.UpdateTaxRecord(t.Context(), id, 2, api.PatchTaxRecordRequest{ClearAuthority: true, ClearTaxYear: true, ClearPeriodStart: true, ClearDueDate: true, ClearLeadDays: true, ClearAmount: true})
	require.NoError(t, err)
}
func TestTaxRecordClientRejectsUntrustedResponses(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct{ body, etag string }{{taxRecordFixture, `"2"`}, {strings.Replace(taxRecordFixture, id, "invalid", 1), `"1"`}, {`{}`, `"1"`}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", tc.etag)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(server.URL, "").TaxRecord(t.Context(), id)
		require.Error(t, err)
		server.Close()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":null,"limit":1,"offset":0}`))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").TaxRecords(t.Context(), "", "", "", nil, "", "", false, 1, 0)
	require.Error(t, err)
}
func TestTaxRecordMutationHint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"wrong"`)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(taxRecordFixture))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").CreateTaxRecord(t.Context(), api.CreateTaxRecordRequest{Kind: "filing", Title: "Example"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inspect the record")
	assert.NotContains(t, err.Error(), "account")
}

func TestTaxRecordDiagnosticsRemainPrivate(t *testing.T) {
	for _, detail := range []string{"title: invalid length or text", "kind: unknown kind", "state: unknown state", "direction: unknown direction", "amount: invalid precision", "amount: amount required when currency changes", "period_end: precedes period_start"} {
		err := safeTaxError(apiProblemError(api.Error{Status: 422, Code: "invalid_tax", Detail: detail + " private-synthetic"}))
		assert.Contains(t, err.Error(), detail)
		assert.NotContains(t, err.Error(), "private-synthetic")
	}
	err := safeTaxError(apiProblemError(api.Error{Status: 422, Code: "invalid_tax", Detail: "private-synthetic-field: private-synthetic"}))
	assert.NotContains(t, err.Error(), "private-synthetic")
}

func TestTaxRecordUncertainWritesNameRecord(t *testing.T) {
	err := taxMutationFailure(&responseDecodeError{err: errors.New("private-synthetic")}, "record")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inspect the record")
	assert.NotContains(t, err.Error(), "private-synthetic")
}
