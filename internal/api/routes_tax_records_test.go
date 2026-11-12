package api_test

import (
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestTaxRecordHTTPWorkflow(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	_, account := createTaxTestAccount(t, ts, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example return", "direction": "payable", "amount": "1234.00"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &record))
	assert.Equal(t, "USD", record["currency"])
	assert.Equal(t, "expected", record["state"])
	assert.Equal(t, "unverified", record["verification"])
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	id := record["record_id"].(string)
	path := "/api/v1/tax/records/" + id
	resp, body = get(t, ts, "/api/v1/tax/records?country=zz&taxkind=example", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, id)
	resp, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	resp, body = do(t, ts, http.MethodPost, path+"/retire", map[string]string{"If-Match": `"1"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/tax/records", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, id)
}

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
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example", "tax_year": 2026, "period_start": "2026-01-01", "period_end": "2026-12-31"})
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
	resp, body := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"currency": "JPY", "amount": "1234"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var resent api.TaxRecord
	require.NoError(t, json.Unmarshal([]byte(body), &resent))
	require.NotNil(t, resent.AmountMinor)
	assert.Equal(t, int64(1234), *resent.AmountMinor)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"2"`}, map[string]any{"currency": "USD", "amount": "12.34"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &resent))
	require.NotNil(t, resent.AmountMinor)
	assert.Equal(t, int64(1234), *resent.AmountMinor)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"3"`}, map[string]any{"currency": "usd", "note": "same magnitude"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &resent))
	require.NotNil(t, resent.AmountMinor)
	assert.Equal(t, int64(1234), *resent.AmountMinor)
	for _, patch := range []map[string]any{{"amount": nil}, {"amount": nil, "direction": "nil"}, {"amount": "1", "direction": "unknown"}, {"amount": "1.234"}, {"amount": "1e3"}, {"amount": "92233720368547758.08"}} {
		before := readTaxTestRecord(t, ts, r.RecordID)
		resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"4"`}, patch)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.Equal(t, before, readTaxTestRecord(t, ts, r.RecordID))
	}
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"4"`}, map[string]any{"amount": nil, "direction": "unknown"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &resent))
	assert.Nil(t, resent.AmountMinor)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"5"`}, map[string]any{"direction": "payable"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"5"`}, map[string]any{"direction": "nil", "amount": "0"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	zero := readTaxTestRecord(t, ts, r.RecordID)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"6"`}, map[string]any{"amount": nil})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Equal(t, zero, readTaxTestRecord(t, ts, r.RecordID))
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"6"`}, map[string]any{"currency": "JPY"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Equal(t, zero, readTaxTestRecord(t, ts, r.RecordID))

}

func TestTaxRecordRevisionGateAndAudit(t *testing.T) {
	t.Parallel()
	gate := api.NewOperationGate()
	ts, fixture := newTestServer(t, func(d *api.Deps) { d.Gate = gate })
	_, a := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": a.AccountID, "kind": "filing", "title": "Example"})
	path := "/api/v1/tax/records/" + r.RecordID
	for _, tc := range []struct {
		match  string
		status int
	}{{"", 428}, {"private-header", 400}, {`"9"`, 412}} {
		headers := map[string]string{}
		if tc.match != "" {
			headers["If-Match"] = tc.match
		}
		resp, body := do(t, ts, http.MethodPatch, path, headers, map[string]any{"note": "example"})
		assert.Equal(t, tc.status, resp.StatusCode, body)
		assert.NotContains(t, body, "private-header")
		assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	go func() {
		done <- gate.MaintainContext(t.Context(), func() error { close(started); <-release; return nil })
	}()
	<-started
	resp, body := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"note": "example"})
	once.Do(func() { close(release) })
	require.NoError(t, <-done)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, body)
	assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
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
		resp, body = do(t, ts, op.method, op.path, op.headers, op.input)
		assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
		assert.Contains(t, body, "audit_mutation_unsupported")
		assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	}
}

func TestTaxRecordCancellationAndPrivacy(t *testing.T) {
	t.Parallel()
	logFile, err := os.CreateTemp(t.TempDir(), "tax-log-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = logFile.Close() })
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Logger = slog.New(slog.NewTextHandler(logFile, nil)) })
	filer, _ := createTaxTestAccount(t, ts, nil)
	const secret = "private-synthetic-record-value"
	createTaxTestRecord(t, ts, map[string]any{"filer_party_id": filer.PartyID, "country": "ZZ", "tax_kind": secret, "kind": "filing", "title": secret, "note": secret})
	for _, field := range []string{"account_id", "filer_party_id", "authority_party_id", "kind", "title", "country", "subdivision", "tax_kind", "form_code", "tax_year", "period_start", "period_end", "due_date", "lead_days", "direction", "amount", "currency", "note", "state", "verification", "revision"} {
		resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, map[string]any{field: []any{secret}})
		assert.GreaterOrEqual(t, resp.StatusCode, 400, field)
		assert.NotContains(t, body, secret, field)
	}
	for _, input := range []any{[]any{secret}, map[string]any{secret: secret}, map[string]any{"kind": "filing", "title": secret, "amount": strings.Repeat(secret, 100)}, map[string]any{"kind": "filing", "title": secret, "tax_year": 10000}} {
		resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, input)
		assert.GreaterOrEqual(t, resp.StatusCode, 400)
		assert.NotContains(t, body, secret)
	}
	for _, q := range []string{"year=" + secret, "state=" + secret, "kind=" + secret, "taxkind=" + secret, "filer=" + secret} {
		resp, body := get(t, ts, "/api/v1/tax/records?"+q, nil)
		if resp.StatusCode >= 400 {
			assert.NotContains(t, body, secret)
		}
	}
	resp, body := do(t, ts, http.MethodPatch, "/api/v1/tax/records/"+secret, map[string]string{"If-Match": secret}, map[string]any{"note": secret})
	assert.GreaterOrEqual(t, resp.StatusCode, 400)
	assert.NotContains(t, body, secret)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/tax/records", strings.NewReader(`{"title":"`+secret))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	bad, err := ts.Client().Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(bad.Body)
	require.NoError(t, err)
	require.NoError(t, bad.Body.Close())
	assert.NotContains(t, string(raw), secret)
	content, err := os.ReadFile(logFile.Name())
	require.NoError(t, err)
	assert.NotContains(t, string(content), secret)
	assert.NotContains(t, string(content), "Example Holdings Ltd.")
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
		{"tax_year": 10000}, {"lead_days": 367}, {"title": strings.Repeat(secret, 100)}, {"note": strings.Repeat(secret, 1000)},
		{"due_date": secret}, {"period_start": "2026-02-01", "period_end": "2026-01-01"}, {"amount": secret, "direction": "payable"},
		{"state": secret}, {"verification": secret}, {"revision": 123}, {"kind": secret}, {"account_id": account.AccountID}, {secret: secret},
	} {
		resp, body := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, patch)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.NotContains(t, body, secret)
		assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	}
}
