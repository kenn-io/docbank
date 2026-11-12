package daemonconn

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/taxrecord"
)

const taxAccountFixture = `{"account_id":"11111111-1111-4111-8111-111111111111","filer_party_id":"22222222-2222-4222-8222-222222222222","authority_party_id":null,"country":"ZZ","subdivision":"","tax_kind":"Example","identifier":"","cadence":"annual","period_note":"","due_note":"","lead_days":0,"currency":"USD","effective_start":null,"effective_end":null,"note":"","retired_at":null,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z","revision":1}`

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
	failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, sentinel, http.StatusInternalServerError) }))
	t.Cleanup(failure.Close)
	_, err := New(failure.URL, "").TaxAccounts(t.Context(), "", "", sentinel, false, 100, 0)
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
		{"different identity", strings.Replace(taxAccountFixture, id, "33333333-3333-4333-8333-333333333333", 1), `"1"`},
		{"zero revision", strings.Replace(taxAccountFixture, `"revision":1`, `"revision":0`, 1), `"0"`},
		{"missing etag", taxAccountFixture, ""},
		{"unknown cadence", strings.Replace(taxAccountFixture, `"annual"`, `"future"`, 1), `"1"`},
		{"noncanonical country", strings.Replace(taxAccountFixture, `"ZZ"`, `"zz"`, 1), `"1"`},
		{"invalid currency", strings.Replace(taxAccountFixture, `"USD"`, `"???"`, 1), `"1"`},
		{"noncanonical kind", strings.Replace(taxAccountFixture, `"Example"`, `" Example "`, 1), `"1"`},
		{"invalid date", strings.Replace(taxAccountFixture, `"effective_start":null`, `"effective_start":"2025-02-30"`, 1), `"1"`},
		{"masked control", strings.Replace(taxAccountFixture, `"identifier":""`, `"identifier":"****123\u0000"`, 1), `"1"`},
		{"updated before created", strings.Replace(taxAccountFixture, `"updated_at":"2025-01-01T00:00:00Z"`, `"updated_at":"2024-01-01T00:00:00Z"`, 1), `"1"`},
		{"retired after updated", strings.Replace(taxAccountFixture, `"retired_at":null`, `"retired_at":"2026-01-01T00:00:00Z"`, 1), `"1"`},
		{"retired before created", strings.Replace(taxAccountFixture, `"retired_at":null`, `"retired_at":"2024-01-01T00:00:00Z"`, 1), `"1"`},
		{"invalid timestamp", strings.Replace(taxAccountFixture, `2025-01-01T00:00:00Z`, `private-synthetic-value`, 1), `"1"`},
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
	require.ErrorIs(t, err, taxrecord.ErrInvalid)
	untrusted := safeTaxError(apiProblemError(api.Error{Status: http.StatusUnprocessableEntity, Code: "invalid_tax", Detail: "cadence: unknown cadence private-synthetic-value"}))
	assert.Contains(t, untrusted.Error(), "cadence: unknown cadence")
	assert.NotContains(t, untrusted.Error(), "private-synthetic-value")
	audit := safeTaxError(apiProblemError(api.Error{Status: http.StatusConflict, Code: "audit_mutation_unsupported", Detail: "private-synthetic-value"}))
	assert.Contains(t, audit.Error(), "tax writes are unavailable in audited vaults")
	assert.NotContains(t, audit.Error(), "private-synthetic-value")
}

func TestTaxClientProblemCauses(t *testing.T) {
	for _, tc := range []struct {
		code  string
		cause error
	}{
		{"tax_revision_exhausted", store.ErrTaxRevisionExhausted},
		{"tax_retired", store.ErrTaxRetired}, {"invalid_tax", taxrecord.ErrInvalid},
	} {
		err := safeTaxError(apiProblemError(api.Error{Status: http.StatusConflict, Code: tc.code, Detail: "private-synthetic-value"}))
		require.ErrorIs(t, err, tc.cause)
		assert.Contains(t, err.Error(), tc.code)
		assert.NotContains(t, err.Error(), "private-synthetic-value")
	}
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
	_, err := c.CreateTaxAccount(t.Context(), api.CreateTaxAccountRequest{})
	require.True(t, IsResponseDecodeError(err))
	assert.Contains(t, err.Error(), "may have been applied")
	assert.Equal(t, 1, creates)
}

func TestTaxClientTransportErrorClass(t *testing.T) {
	err := taxMutationFailure(&transportError{err: errors.Join(context.DeadlineExceeded, errors.New("private-synthetic-value"))}, "account")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, IsTransportError(err))
	assert.Contains(t, err.Error(), "deadline exceeded")
	assert.Contains(t, err.Error(), "may have been applied")
	assert.NotContains(t, err.Error(), "private-synthetic-value")
}

// Each operation gets an otherwise valid receipt, so only the deleted member can fail it.
func TestTaxClientRejectsMissingAccountMembers(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	second := strings.Replace(taxAccountFixture, `"revision":1`, `"revision":2`, 1)
	for _, receipt := range []struct{ operation, body, etag string }{
		{"get", taxAccountFixture, `"1"`},
		{"list", taxAccountFixture, `"1"`},
		{"create", taxAccountFixture, `"1"`},
		{"update", second, `"2"`},
		{"retire", strings.Replace(second, `"retired_at":null`, `"retired_at":"2025-01-01T00:00:00Z"`, 1), `"2"`},
	} {
		// The empty name is the intact control.
		for _, field := range []string{"", "account_id", "filer_party_id", "authority_party_id", "country", "subdivision", "tax_kind", "identifier", "cadence", "period_note", "due_note", "lead_days", "currency", "effective_start", "effective_end", "note", "revision", "created_at", "updated_at", "retired_at"} {
			t.Run(receipt.operation+"/"+field, func(t *testing.T) {
				var members map[string]any
				require.NoError(t, json.Unmarshal([]byte(receipt.body), &members))
				delete(members, field)
				body, err := json.Marshal(members)
				require.NoError(t, err)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("ETag", receipt.etag)
					if r.Method == http.MethodPost && r.URL.Path == "/api/v1/tax/accounts" {
						w.WriteHeader(http.StatusCreated)
					}
					if r.Method == http.MethodGet && r.URL.Path == "/api/v1/tax/accounts" {
						_, _ = w.Write([]byte(`{"items":[` + string(body) + `],"limit":100,"offset":0}`))
						return
					}
					_, _ = w.Write(body)
				}))
				t.Cleanup(server.Close)
				c := New(server.URL, "")
				switch receipt.operation {
				case "get":
					_, err = c.TaxAccount(t.Context(), id, false)
				case "list":
					_, err = c.TaxAccounts(t.Context(), "", "", "", false, 100, 0)
				case "create":
					_, err = c.CreateTaxAccount(t.Context(), api.CreateTaxAccountRequest{})
				case "update":
					_, err = c.UpdateTaxAccount(t.Context(), id, 1, api.PatchTaxAccountRequest{Note: new("")})
				default:
					_, err = c.RetireTaxAccount(t.Context(), id, 1)
				}
				if field == "" {
					require.NoError(t, err)
					return
				}
				require.True(t, IsResponseDecodeError(err))
				if receipt.operation != "get" && receipt.operation != "list" {
					assert.Contains(t, err.Error(), "may have been applied")
				}
			})
		}
	}
}

func TestTaxReceiptBoundAndMetadata(t *testing.T) {
	body := strings.Replace(taxAccountFixture, `"identifier":""`, `"identifier":"****1234","$schema":"example","extension":true`, 1)
	response := func(s string) *http.Response { return &http.Response{Body: io.NopCloser(strings.NewReader(s))} }
	// Valid JSON padded with whitespace isolates the single-account size limit.
	padded := body + strings.Repeat(" ", maxTaxAccountReceiptBytes-len(body))
	exact := response(padded)
	defer func() { _ = exact.Body.Close() }()
	_, err := decodeTaxReceipt(t.Context(), exact, false)
	require.NoError(t, err)
	large := response(padded + " ")
	defer func() { _ = large.Body.Close() }()
	_, err = decodeTaxReceipt(t.Context(), large, false)
	assert.True(t, IsResponseDecodeError(err))
}

func TestTaxMutationReceiptTransitions(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const retired = `"retired_at":"2025-01-01T00:00:00Z"`
	const retire = "retire"
	for _, tc := range []struct {
		name, method, body, etag string
		revision                 int64
		valid                    bool
	}{
		{"create", http.MethodPost, taxAccountFixture, `"1"`, 0, true},
		{"create wrong revision", http.MethodPost, strings.Replace(taxAccountFixture, `"revision":1`, `"revision":2`, 1), `"2"`, 0, false},
		{"create retired", http.MethodPost, strings.Replace(taxAccountFixture, `"retired_at":null`, retired, 1), `"1"`, 0, false},
		{"edit", http.MethodPatch, strings.Replace(taxAccountFixture, `"revision":1`, `"revision":2`, 1), `"2"`, 1, true},
		{"edit stale receipt", http.MethodPatch, taxAccountFixture, `"1"`, 1, false},
		{"edit retired", http.MethodPatch, strings.Replace(strings.Replace(taxAccountFixture, `"revision":1`, `"revision":2`, 1), `"retired_at":null`, retired, 1), `"2"`, 1, false},
		{"retire", retire, strings.Replace(strings.Replace(taxAccountFixture, `"revision":1`, `"revision":2`, 1), `"retired_at":null`, retired, 1), `"2"`, 1, true},
		{"retire missing timestamp", retire, strings.Replace(taxAccountFixture, `"revision":1`, `"revision":2`, 1), `"2"`, 1, false},
		{"retire stale receipt", retire, strings.Replace(taxAccountFixture, `"retired_at":null`, retired, 1), `"1"`, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.method == retire {
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Equal(t, "/api/v1/tax/accounts/"+id+"/retire", r.URL.Path)
				} else {
					assert.Equal(t, tc.method, r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("ETag", tc.etag)
				if tc.method == http.MethodPost {
					w.WriteHeader(http.StatusCreated)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			c := New(server.URL, "")
			var err error
			switch tc.method {
			case http.MethodPost:
				_, err = c.CreateTaxAccount(t.Context(), api.CreateTaxAccountRequest{})
			case http.MethodPatch:
				_, err = c.UpdateTaxAccount(t.Context(), id, tc.revision, api.PatchTaxAccountRequest{Note: new("")})
			case retire:
				_, err = c.RetireTaxAccount(t.Context(), id, tc.revision)
			}
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.True(t, IsResponseDecodeError(err))
			assert.Contains(t, err.Error(), "may have been applied")
		})
	}
}

func TestTaxMutationMaintenanceIsDefinite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":503,"code":"maintenance_busy","detail":"private-synthetic-value"}`))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "").CreateTaxAccount(t.Context(), api.CreateTaxAccountRequest{})
	require.ErrorIs(t, err, ErrMaintenanceBusy)
	assert.Contains(t, err.Error(), "retry this mutation after maintenance finishes")
	assert.NotContains(t, err.Error(), "may have been applied")
	assert.NotContains(t, err.Error(), "private-synthetic-value")
	// Unknown server failures over real HTTP keep their status and stay uncertain.
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
		failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"status":%d,"code":"internal","detail":"private-synthetic-value"}`, status)
		}))
		t.Cleanup(failure.Close)
		_, err = New(failure.URL, "").CreateTaxAccount(t.Context(), api.CreateTaxAccountRequest{})
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrMaintenanceBusy)
		assert.Contains(t, err.Error(), fmt.Sprintf("HTTP %d", status))
		assert.Contains(t, err.Error(), "may have been applied")
		assert.NotContains(t, err.Error(), "private-synthetic-value")
	}
}

func TestTaxReceiptIdentifierContexts(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		identifier    string
		reveal, valid bool
	}{
		{"****1234", false, true}, {"****", false, true}, {"synthetic12345", false, false},
		{"synthetic12345", true, true}, {"synthetic\x00", true, false},
	} {
		body, err := json.Marshal(tc.identifier)
		require.NoError(t, err)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"1"`)
			_, _ = w.Write([]byte(strings.Replace(taxAccountFixture, `"identifier":""`, `"identifier":`+string(body), 1)))
		}))
		t.Cleanup(server.Close)
		_, err = New(server.URL, "").TaxAccount(t.Context(), id, tc.reveal)
		if tc.valid {
			assert.NoError(t, err, tc.identifier)
		} else {
			assert.True(t, IsResponseDecodeError(err), tc.identifier)
		}
	}
}
