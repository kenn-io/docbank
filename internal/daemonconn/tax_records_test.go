package daemonconn

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

const taxRecordFixture = `{"record_id":"11111111-1111-4111-8111-111111111111","filer_party_id":"22222222-2222-4222-8222-222222222222","account_id":null,"authority_party_id":null,"subdivision":"","form_code":"","tax_year":null,"period_start":null,"period_end":null,"due_date":null,"lead_days":null,"amount_minor":null,"currency":"","verified_on":null,"verified_revision":null,"verification_note":"","note":"","retired_at":null,"kind":"filing","title":"Example","country":"ZZ","tax_kind":"Example","state":"expected","direction":"unknown","verification":"unverified","revision":1,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","$schema":"https://example.com/schema","extra":true}`

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
	for _, tc := range []struct{ body, etag string }{{taxRecordFixture, `"2"`}, {strings.Replace(taxRecordFixture, id, "invalid", 1), `"1"`}, {strings.Replace(taxRecordFixture, `"kind":"filing"`, `"kind":"invalid"`, 1), `"1"`}, {strings.Replace(taxRecordFixture, `"state":"expected"`, `"state":"filed"`, 1), `"1"`}, {strings.Replace(taxRecordFixture, `"updated_at":"2025-01-01T00:00:00Z"`, `"updated_at":"invalid"`, 1), `"1"`}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", tc.etag)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(server.URL, "").TaxRecord(t.Context(), id)
		require.Error(t, err)
		server.Close()
	}

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
	for _, detail := range []string{"title: invalid length or text"} {
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

func TestTaxRecordClientRejectsMissingAndNullMembers(t *testing.T) {
	t.Parallel()
	const id = "11111111-1111-4111-8111-111111111111"
	fields := []string{"record_id", "account_id", "filer_party_id", "authority_party_id", "kind", "title", "country", "subdivision", "tax_kind", "form_code", "tax_year", "period_start", "period_end", "due_date", "lead_days", "state", "direction", "amount_minor", "currency", "verification", "verified_on", "verified_revision", "verification_note", "note", "revision", "created_at", "updated_at", "retired_at"}
	nullable := map[string]bool{"account_id": true, "authority_party_id": true, "tax_year": true, "period_start": true, "period_end": true, "due_date": true, "lead_days": true, "amount_minor": true, "verified_on": true, "verified_revision": true, "retired_at": true}
	for _, field := range fields {
		for _, null := range []bool{false, true} {
			if null && nullable[field] {
				continue
			}
			t.Run(field+fmt.Sprint(null), func(t *testing.T) {
				t.Parallel()
				var members map[string]any
				require.NoError(t, json.Unmarshal([]byte(taxRecordFixture), &members))
				if null {
					members[field] = nil
				} else {
					delete(members, field)
				}
				body, err := json.Marshal(members)
				require.NoError(t, err)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("ETag", `"1"`)
					if r.Method == http.MethodGet && r.URL.Path == "/api/v1/tax/records" {
						_, _ = fmt.Fprintf(w, `{"items":[%s],"limit":1,"offset":0}`, body)
						return
					}
					_, _ = w.Write(body)
				}))
				t.Cleanup(server.Close)
				c := New(server.URL, "")
				_, err = c.TaxRecord(t.Context(), id)
				require.True(t, IsResponseDecodeError(err))

			})
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"1"`)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"items":[{}],"limit":1,"offset":0}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "")
	_, err := c.TaxRecords(t.Context(), "", "", "", nil, "", "", false, 1, 0)
	require.True(t, IsResponseDecodeError(err))
	_, err = c.CreateTaxRecord(t.Context(), api.CreateTaxRecordRequest{})
	require.True(t, IsResponseDecodeError(err))
	_, err = c.UpdateTaxRecord(t.Context(), id, 1, api.PatchTaxRecordRequest{})
	require.True(t, IsResponseDecodeError(err))
	_, err = c.RetireTaxRecord(t.Context(), id, 1)
	require.True(t, IsResponseDecodeError(err))

	for _, body := range []string{`{}`, `{"items":[],"offset":0}`, `{"items":[],"limit":1}`, `{"items":null,"limit":1,"offset":0}`, `{"items":[],"limit":null,"offset":0}`, `{"items":[],"limit":1,"offset":null}`} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)
			_, err := New(server.URL, "").TaxRecords(t.Context(), "", "", "", nil, "", "", false, 1, 0)
			require.True(t, IsResponseDecodeError(err))
		})
	}
}
