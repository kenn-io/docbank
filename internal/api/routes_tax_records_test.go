package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func createTaxTestRecord(t *testing.T, ts *httptest.Server, input map[string]any) api.TaxRecord {
	t.Helper()
	resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, input)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var r api.TaxRecord
	require.NoError(t, json.Unmarshal([]byte(body), &r))
	return r
}

func readTaxTestRecord(t *testing.T, ts *httptest.Server, id string) api.TaxRecord {
	t.Helper()
	resp, body := get(t, ts, "/api/v1/tax/records/"+id, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var r api.TaxRecord
	require.NoError(t, json.Unmarshal([]byte(body), &r))
	return r
}

func TestTaxRecordFiltersAndReferences(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	filer, account := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example", "tax_year": 2026, "period_start": "2026-01-01", "period_end": "2026-12-31", "direction": "payable", "amount": "1.00"})
	assert.Equal(t, "USD", r.Currency)
	initial, initialBody := get(t, ts, "/api/v1/tax/records/"+r.RecordID, nil)
	require.Equal(t, http.StatusOK, initial.StatusCode, initialBody)
	assert.Equal(t, `"1"`, initial.Header.Get("ETag"))
	for _, q := range []string{"country=zz", "kind=filing", "year=2026", "state=expected", "taxkind=example", "filer=" + filer.PartyID} {
		resp, body := get(t, ts, "/api/v1/tax/records?"+q, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Contains(t, body, r.RecordID)
	}
	for _, q := range []string{"kind=audit", "year=2025", "state=opened", "taxkind=other", "filer=33333333-3333-4333-8333-333333333333"} {
		resp, body := get(t, ts, "/api/v1/tax/records?"+q, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.NotContains(t, body, r.RecordID)
	}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Duplicate", "period_start": "2026-01-01", "period_end": "2026-12-31"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	for _, q := range []string{"filer=bad", "country=%C5%BFZ", "country=bad", "year=0", "year=10000", "year=bad", "state=bad", "kind=bad", "limit=0", "limit=1001", "offset=-1"} {
		resp, body := get(t, ts, "/api/v1/tax/records?"+q, nil)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/records/"+r.RecordID+"/retire", map[string]string{"If-Match": `"1"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	retired := readTaxTestRecord(t, ts, r.RecordID)
	resp, body = do(t, ts, http.MethodPatch, "/api/v1/tax/records/"+r.RecordID, map[string]string{"If-Match": `"2"`}, map[string]any{"currency": "JPY"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, "tax_retired")
	assert.Equal(t, retired, readTaxTestRecord(t, ts, r.RecordID))
	resp, body = do(t, ts, http.MethodPatch, "/api/v1/tax/records/"+r.RecordID, map[string]string{"If-Match": `"1"`}, map[string]any{"currency": "JPY"})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	_, activeBody := get(t, ts, "/api/v1/tax/records", nil)
	assert.NotContains(t, activeBody, r.RecordID)
	for _, op := range []struct {
		method, path string
		body         any
	}{{http.MethodPatch, "/api/v1/tax/records/" + r.RecordID, map[string]any{"note": "example"}}, {http.MethodPost, "/api/v1/tax/records/" + r.RecordID + "/retire", nil}} {
		resp, body = do(t, ts, op.method, op.path, map[string]string{"If-Match": `"2"`}, op.body)
		assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
		assert.Contains(t, body, "tax_retired")
		assert.Equal(t, retired, readTaxTestRecord(t, ts, r.RecordID))
	}
	resp, body = get(t, ts, "/api/v1/tax/records?include_retired=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, r.RecordID)
}

func TestTaxRecordAccountIdentityDiagnostics(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	_, account := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example"})
	_, before := get(t, ts, "/api/v1/tax/records", nil)
	for field, value := range map[string]string{"filer_party_id": "33333333-3333-4333-8333-333333333333", "country": "US", "subdivision": "ZZ-BC", "authority_party_id": "44444444-4444-4444-8444-444444444444", "tax_kind": "PrivateSynthetic"} {
		create := map[string]any{"account_id": account.AccountID, "kind": "notice", "title": "Example", field: value}
		resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, create)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.Contains(t, body, field+": account identity mismatch")
		assert.NotContains(t, body, value)
		resp, body = do(t, ts, http.MethodPatch, "/api/v1/tax/records/"+r.RecordID, map[string]string{"If-Match": `"1"`}, map[string]any{field: value})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.Contains(t, body, field+": account identity mismatch")
		assert.NotContains(t, body, value)
		assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, map[string]any{"account_id": account.AccountID, "kind": "notice", "title": "Example", "country": "US", "tax_kind": "PrivateSynthetic"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, "country: account identity mismatch")
	_, after := get(t, ts, "/api/v1/tax/records", nil)
	assert.Equal(t, before, after)
}

func TestTaxRecordAuthorityAndPatchPresence(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "Example Authority"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var authority api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &authority))
	filer, account := createTaxTestAccount(t, ts, map[string]any{"authority_party_id": authority.PartyID})
	attached := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Attached"})
	require.NotNil(t, attached.AuthorityPartyID)
	assert.Equal(t, authority.PartyID, *attached.AuthorityPartyID)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, map[string]any{"account_id": account.AccountID, "kind": "notice", "title": "Mismatch", "authority_party_id": nil})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	one := createTaxTestRecord(t, ts, map[string]any{"filer_party_id": filer.PartyID, "country": "ZZ", "tax_kind": "Example", "kind": "notice", "title": "One-off", "authority_party_id": authority.PartyID, "form_code": "F1", "period_start": "2026-01-01", "period_end": "2026-12-31", "due_date": "2027-01-31", "lead_days": 10, "direction": "payable", "amount": "12.34", "currency": "USD", "note": "Example note"})
	require.NotNil(t, one.AuthorityPartyID)
	require.NotNil(t, one.TaxYear)
	path := "/api/v1/tax/records/" + one.RecordID
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"period_end": "2027-12-31"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var changed api.TaxRecord
	require.NoError(t, json.Unmarshal([]byte(body), &changed))
	assert.Equal(t, one.Title, changed.Title)
	assert.Equal(t, one.FormCode, changed.FormCode)
	assert.Equal(t, one.Note, changed.Note)
	assert.Equal(t, one.TaxYear, changed.TaxYear)
	assert.Equal(t, one.DueDate, changed.DueDate)
	assert.Equal(t, one.LeadDays, changed.LeadDays)
	assert.Equal(t, one.AuthorityPartyID, changed.AuthorityPartyID)
	assert.Equal(t, one.AmountMinor, changed.AmountMinor)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"2"`}, map[string]any{"authority_party_id": nil, "tax_year": nil, "period_start": nil, "period_end": nil, "due_date": nil, "lead_days": nil, "amount": nil, "direction": "unknown"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &changed))
	assert.Nil(t, changed.AuthorityPartyID)
	assert.Nil(t, changed.TaxYear)
	assert.Nil(t, changed.PeriodStart)
	assert.Nil(t, changed.PeriodEnd)
	assert.Nil(t, changed.DueDate)
	assert.Nil(t, changed.LeadDays)
	assert.Nil(t, changed.AmountMinor)
}

func TestTaxRecordMoneyHTTP(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	filer, _ := createTaxTestAccount(t, ts, nil)
	create := func(currency, amount, direction string) api.TaxRecord {
		return createTaxTestRecord(t, ts, map[string]any{"filer_party_id": filer.PartyID, "country": "ZZ", "tax_kind": "Example", "kind": "filing", "title": "Money example", "currency": currency, "direction": direction, "amount": amount})
	}
	r := create("USD", "1234.00", "payable")
	path := "/api/v1/tax/records/" + r.RecordID
	omittedAmount, body := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"currency": "JPY"})
	assert.Equal(t, 422, omittedAmount.StatusCode, body)
	assert.Contains(t, body, `"code":"invalid_tax"`)
	assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	stale, staleBody := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"9"`}, map[string]any{"currency": "JPY"})
	assert.Equal(t, http.StatusPreconditionFailed, stale.StatusCode, staleBody)
	assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	bad, badBody := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"amount": "1.234"})
	assert.Equal(t, http.StatusUnprocessableEntity, bad.StatusCode, badBody)
	assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
}

func TestTaxRecordAuditRefusal(t *testing.T) {
	t.Parallel()
	ts, fixture := newTestServer(t, nil)
	_, a := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": a.AccountID, "kind": "filing", "title": "Example"})
	path := "/api/v1/tax/records/" + r.RecordID
	plan, err := fixture.PreviewInitialAudit(t.Context(), fixture.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = fixture.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	for _, op := range []struct {
		method, path string
		input        any
		headers      map[string]string
	}{
		{http.MethodPost, "/api/v1/tax/records", map[string]any{"account_id": a.AccountID, "kind": "notice", "title": "New"}, nil},
		{http.MethodPatch, path, map[string]any{"note": "example"}, map[string]string{"If-Match": `"1"`}},
		{http.MethodPost, path + "/retire", nil, map[string]string{"If-Match": `"1"`}},
	} {
		resp, body := do(t, ts, op.method, op.path, op.headers, op.input)
		assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
		assert.Contains(t, body, "audit_mutation_unsupported")
		assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	}
}

func TestTaxRecordRejectedPatchesPreserveRow(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	_, account := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example"})
	path := "/api/v1/tax/records/" + r.RecordID
	resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example", "direction": ""})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	const secret = "private-record-value"
	for _, patch := range []map[string]any{
		{}, {"$schema": "https://example.com/schema"}, {"title": nil}, {"currency": nil}, {"direction": nil}, {"direction": ""}, {"note": nil},
		{"filer_party_id": nil}, {"country": nil}, {"subdivision": nil}, {"tax_kind": nil}, {"form_code": nil},
		{"note": "valid", "title": nil}, {"note": "valid", "filer_party_id": nil}, {"note": "valid", "country": nil}, {"note": "valid", "subdivision": nil}, {"note": "valid", "tax_kind": nil}, {"note": "valid", "form_code": nil}, {"note": "valid", "direction": nil}, {"note": "valid", "currency": nil}, {"NOTE": secret}, {"Note": nil}, {"DUE_DATE": nil}, {"note": "valid", secret: secret},
		{"tax_year": 10000},
		{"state": secret}, {"verification": secret}, {"revision": 123}, {"kind": secret}, {"account_id": account.AccountID}, {secret: secret},
	} {
		resp, body := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, patch)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.Contains(t, body, `"code":"validation"`)
		assert.NotContains(t, body, secret)
		assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	}
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"due_date": secret})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"invalid_tax"`)
	assert.NotContains(t, body, secret)
	assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	_, beforeCreate := get(t, ts, "/api/v1/tax/records", nil)
	for _, field := range []string{"filer_party_id", "kind", "title", "country", "subdivision", "tax_kind", "form_code", "direction", "currency", "note"} {
		input := map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example", field: nil}
		resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, input)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.Contains(t, body, `"code":"validation"`)
	}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example", "NOTE": secret})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.NotContains(t, body, secret)
	_, afterCreate := get(t, ts, "/api/v1/tax/records", nil)
	assert.Equal(t, beforeCreate, afterCreate)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"due_date": nil, "note": "valid"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, int64(2), readTaxTestRecord(t, ts, r.RecordID).Revision)
}
