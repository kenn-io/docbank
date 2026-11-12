package api_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"math"
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
	for _, patch := range []map[string]any{{}, {"$schema": "https://example.com/schema"}, {"currency": "JPY"}} {
		resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, patch)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		if _, ok := patch["currency"]; !ok {
			assert.Contains(t, body, `"code":"validation"`)
		}
	}
	resp, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	var unchanged map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &unchanged))
	assert.Equal(t, record, unchanged)
	resp, body = do(t, ts, http.MethodPatch, path, nil, map[string]any{"note": "example"})
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"9"`}, map[string]any{"note": "example"})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"currency": "JPY", "amount": "1234"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &record))
	assert.EqualValues(t, 1234, record["amount_minor"])
	resp, body = do(t, ts, http.MethodPost, path+"/retire", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/tax/records", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, id)
}

func TestTaxRecordHTTPValidationRedaction(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	const sentinel = "private-synthetic-record"
	for _, field := range []string{"title", "kind", "form_code", "tax_year", "period_start", "period_end", "due_date", "direction", "amount", "state", "tax_kind", sentinel} {
		resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, map[string]any{field: []any{sentinel}})
		assert.GreaterOrEqual(t, resp.StatusCode, 400, field)
		assert.NotContains(t, body, sentinel, field)
	}
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
	a := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example filing", "period_start": "2026-01-01", "period_end": "2026-12-31"})
	b := createTaxTestRecord(t, ts, map[string]any{"filer_party_id": filer.PartyID, "country": "ZZ", "tax_kind": "Example", "kind": "audit", "title": "Example audit", "tax_year": 2025})
	assert.Equal(t, "expected", a.State)
	assert.Equal(t, "opened", b.State)
	assert.Equal(t, int64(1), a.Revision)
	assert.Equal(t, "unverified", a.Verification)
	require.NotNil(t, a.TaxYear)
	assert.Equal(t, int64(2026), *a.TaxYear)
	for _, q := range []string{"filer=" + filer.PartyID, "country=zz", "taxkind=eXaMpLe", "year=2026", "state=expected", "kind=filing", "filer=" + filer.PartyID + "&country=zz&taxkind=example&year=2026&state=expected&kind=filing"} {
		resp, body := get(t, ts, "/api/v1/tax/records?"+q, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var page api.TaxRecordPage
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		assert.Contains(t, page.Items, a)
	}
	resp, body := get(t, ts, "/api/v1/tax/records?limit=100", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.TaxRecordPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 2)
	assert.Less(t, page.Items[0].RecordID, page.Items[1].RecordID)
	resp, body = get(t, ts, "/api/v1/tax/records?offset=2", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, `"items":[]`)
	for _, q := range []string{"filer=bad", "country=%C5%BFZ", "country=bad", "year=0", "year=10000", "year=bad", "state=bad", "kind=bad", "limit=0", "limit=1001", "offset=-1"} {
		resp, body = get(t, ts, "/api/v1/tax/records?"+q, nil)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
	resp, body = do(t, ts, http.MethodPatch, "/api/v1/tax/accounts/"+account.AccountID, map[string]string{"If-Match": `"1"`}, map[string]any{"currency": "JPY"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, "USD", readTaxTestRecord(t, ts, a.RecordID).Currency)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/accounts/"+account.AccountID+"/retire", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/parties/"+filer.PartyID+"/retire", map[string]string{"If-Match": `"1"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, "/api/v1/tax/records/"+a.RecordID, map[string]string{"If-Match": `"1"`}, map[string]any{"note": "historical"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, map[string]any{"account_id": account.AccountID, "kind": "notice", "title": "New"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, map[string]any{"filer_party_id": filer.PartyID, "country": "ZZ", "tax_kind": "Example", "kind": "notice", "title": "New"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/records/"+a.RecordID+"/retire", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/tax/records", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, a.RecordID)
	resp, body = get(t, ts, "/api/v1/tax/records?include_retired=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, a.RecordID)
	assert.NotNil(t, readTaxTestRecord(t, ts, a.RecordID).RetiredAt)
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
	for _, patch := range []map[string]any{{"kind": "audit"}, {"account_id": account.AccountID}, {"state": "filed"}, {"verification": "verified"}, {"revision": 10}, {"title": nil}} {
		before := readTaxTestRecord(t, ts, one.RecordID)
		resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"3"`}, patch)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.Equal(t, before, readTaxTestRecord(t, ts, one.RecordID))
	}
	resp, body = do(t, ts, http.MethodPatch, "/api/v1/tax/records/"+attached.RecordID, map[string]string{"If-Match": `"1"`}, map[string]any{"country": "CA"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"3"`}, map[string]any{"country": "CA", "authority_party_id": authority.PartyID})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
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
	for _, tc := range []struct {
		currency, amount string
		minor            int64
	}{{"BHD", "1.234", 1234}, {"USD", "92233720368547758.07", math.MaxInt64}} {
		v := create(tc.currency, tc.amount, "payable")
		resp, body = do(t, ts, http.MethodPatch, "/api/v1/tax/records/"+v.RecordID, map[string]string{"If-Match": `"1"`}, map[string]any{"note": "preserved"})
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var got api.TaxRecord
		require.NoError(t, json.Unmarshal([]byte(body), &got))
		require.NotNil(t, got.AmountMinor)
		assert.Equal(t, tc.minor, *got.AmountMinor)
	}
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
	r := createTaxTestRecord(t, ts, map[string]any{"filer_party_id": filer.PartyID, "country": "ZZ", "tax_kind": secret, "kind": "filing", "title": secret, "note": secret})
	path := "/api/v1/tax/records/" + r.RecordID
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req, err = http.NewRequestWithContext(ctx, http.MethodPatch, ts.URL+path, bytes.NewBufferString(`{"note":"changed"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"1"`)
	_, err = ts.Client().Do(req)
	require.Error(t, err)
	assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
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
	const secret = "private-record-value"
	for _, patch := range []map[string]any{
		{}, {"$schema": "https://example.com/schema"}, {"title": nil}, {"currency": nil}, {"direction": nil}, {"note": nil},
		{"tax_year": 10000}, {"lead_days": 367}, {"title": strings.Repeat(secret, 100)}, {"note": strings.Repeat(secret, 1000)},
		{"due_date": secret}, {"period_start": "2026-02-01", "period_end": "2026-01-01"}, {"amount": secret, "direction": "payable"},
		{"state": secret}, {"verification": secret}, {"revision": 123}, {"kind": secret}, {secret: secret},
	} {
		resp, body := do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, patch)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.NotContains(t, body, secret)
		assert.Equal(t, r, readTaxTestRecord(t, ts, r.RecordID))
	}
}

func TestTaxRecordKindNormalizationBoundary(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	filer, _ := createTaxTestAccount(t, ts, nil)
	body := map[string]any{"filer_party_id": filer.PartyID, "country": "ZZ", "tax_kind": "  " + strings.Repeat("a", 64) + "  ", "kind": "notice", "title": "Example"}
	resp, raw := do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, body)
	require.Equal(t, http.StatusCreated, resp.StatusCode, raw)
	var record api.TaxRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &record))
	assert.Equal(t, strings.Repeat("a", 64), record.TaxKind)
	body["tax_kind"] = strings.Repeat("a", 65)
	resp, raw = do(t, ts, http.MethodPost, "/api/v1/tax/records", nil, body)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, raw)
}
