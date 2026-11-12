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

func createTaxTestAccount(t *testing.T, ts *httptest.Server, overrides map[string]any) (api.Party, api.TaxAccount) {
	t.Helper()
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "Example Holdings Ltd.", "own": true})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	input := map[string]any{"filer_party_id": party.PartyID, "country": "ZZ", "tax_kind": "Example", "cadence": "annual", "currency": "USD"}
	for name, value := range overrides {
		input[name] = value
	}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/accounts", nil, input)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var account api.TaxAccount
	require.NoError(t, json.Unmarshal([]byte(body), &account))
	return party, account
}

func TestTaxAccountHTTPWorkflow(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "Example Holdings Ltd.", "own": true})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	authorityResp, authorityBody := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "Example Authority"})
	require.Equal(t, http.StatusCreated, authorityResp.StatusCode, authorityBody)
	var authority api.Party
	require.NoError(t, json.Unmarshal([]byte(authorityBody), &authority))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/accounts", nil, map[string]any{"$schema": "https://example.com/schema.json", "filer_party_id": party.PartyID, "authority_party_id": authority.PartyID, "country": "ZZ", "tax_kind": "Example", "cadence": "annual", "currency": "USD", "identifier": "synthetic12345", "effective_start": "2025-01-01"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var account api.TaxAccount
	require.NoError(t, json.Unmarshal([]byte(body), &account))
	require.NotNil(t, account.AuthorityPartyID)
	assert.Equal(t, authority.PartyID, *account.AuthorityPartyID)
	assert.Equal(t, int64(14), account.LeadDays)
	assert.Equal(t, "****2345", account.Identifier)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	path := "/api/v1/tax/accounts/" + account.AccountID
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"$schema": "https://example.com/schema.json"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = get(t, ts, path+"?reveal=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.NoError(t, json.Unmarshal([]byte(body), &account))
	assert.Equal(t, "synthetic12345", account.Identifier)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"$schema": "https://example.com/schema.json", "effective_start": nil, "lead_days": 0})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &account))
	assert.Nil(t, account.EffectiveStart)
	assert.Zero(t, account.LeadDays)
	assert.Equal(t, "****2345", account.Identifier)
	resp, body = get(t, ts, path+"?reveal=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, "synthetic12345")
	resp, body = do(t, ts, http.MethodPost, path+"/retire", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, "synthetic12345")
	resp, body = get(t, ts, "/api/v1/tax/accounts", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Contains(t, body, `"items":[]`)
	resp, body = get(t, ts, "/api/v1/tax/accounts?include_retired=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, account.AccountID)
	assert.NotContains(t, body, "synthetic12345")
	resp, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Contains(t, body, `"retired_at":`)
	assert.NotContains(t, body, "synthetic12345")
}

func TestTaxHTTPValidationRedaction(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	const sentinel = "private-synthetic-value"
	for _, input := range []any{
		map[string]any{"identifier": sentinel, sentinel: sentinel},
		map[string]any{"identifier": sentinel, "lead_days": sentinel},
		map[string]any{"cadence": sentinel, "lead_days": 367, "country": sentinel},
		map[string]any{"identifier": nil},
		map[string]any{"identifier": strings.Repeat(sentinel, 20)},
		[]any{sentinel},
	} {
		resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/accounts", nil, input)
		assert.GreaterOrEqual(t, resp.StatusCode, 400)
		assert.NotContains(t, body, sentinel)
	}
	for _, field := range []string{"filer_party_id", "authority_party_id", "country", "subdivision", "tax_kind", "identifier", "cadence", "period_note", "due_note", "lead_days", "currency", "effective_start", "effective_end", "note"} {
		input := map[string]any{field: []any{sentinel}}
		resp, body := do(t, ts, http.MethodPost, "/api/v1/tax/accounts", nil, input)
		assert.GreaterOrEqual(t, resp.StatusCode, 400, field)
		assert.NotContains(t, body, sentinel, field)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/tax/accounts", strings.NewReader(`{"identifier":"`+sentinel))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	malformed, err := ts.Client().Do(request)
	require.NoError(t, err)
	data, err := io.ReadAll(malformed.Body)
	require.NoError(t, err)
	require.NoError(t, malformed.Body.Close())
	assert.GreaterOrEqual(t, malformed.StatusCode, 400)
	assert.NotContains(t, string(data), sentinel)
	resp, body := do(t, ts, http.MethodPatch, "/api/v1/tax/accounts/example", map[string]string{"If-Match": sentinel}, map[string]any{"note": "example"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	assert.NotContains(t, body, sentinel)
	resp, body = get(t, ts, "/api/v1/tax/accounts?limit="+sentinel, nil)
	assert.GreaterOrEqual(t, resp.StatusCode, 400)
	assert.NotContains(t, body, sentinel)
}

func TestTaxHTTPRefusals(t *testing.T) {
	gate := api.NewOperationGate()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Gate = gate })
	party, account := createTaxTestAccount(t, ts, nil)
	path := "/api/v1/tax/accounts/" + account.AccountID
	for _, tc := range []struct {
		headers map[string]string
		patch   map[string]any
		status  int
		code    string
	}{
		{nil, map[string]any{"note": "example"}, http.StatusPreconditionRequired, "precondition_required"},
		{map[string]string{"If-Match": `"2"`}, map[string]any{"note": "example"}, http.StatusPreconditionFailed, "stale_revision"},
		{map[string]string{"If-Match": `"1"`}, map[string]any{"country": "ZZ"}, http.StatusUnprocessableEntity, "validation"},
		{map[string]string{"If-Match": `"1"`}, map[string]any{"state": "private"}, http.StatusUnprocessableEntity, "validation"},
		{map[string]string{"If-Match": `"1"`}, map[string]any{"revision": 2}, http.StatusUnprocessableEntity, "validation"},
		{map[string]string{"If-Match": `"1"`}, map[string]any{"identifier": nil}, http.StatusUnprocessableEntity, "validation"},
		{map[string]string{"If-Match": `"1"`}, map[string]any{"lead_days": nil}, http.StatusUnprocessableEntity, "validation"},
	} {
		resp, body := do(t, ts, http.MethodPatch, path, tc.headers, tc.patch)
		assert.Equal(t, tc.status, resp.StatusCode, body)
		assert.Contains(t, body, `"code":"`+tc.code+`"`)
	}
	resp, body := get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &account))
	assert.Equal(t, int64(1), account.Revision)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	go func() {
		done <- gate.MaintainContext(t.Context(), func() error { close(started); <-release; return nil })
	}()
	<-started
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"note": "example"})
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-done)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"maintenance_busy"`)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/parties/"+party.PartyID+"/retire", map[string]string{"If-Match": `"1"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"note": "Historical example"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	input := map[string]any{"filer_party_id": party.PartyID, "country": "ZZ", "tax_kind": "Example", "cadence": "annual", "currency": "USD"}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/accounts", nil, input)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"invalid_tax"`)
	assert.Contains(t, body, "filer_party_id: party is retired")
	resp, body = do(t, ts, http.MethodPost, path+"/retire", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"3"`}, map[string]any{"note": "example"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"tax_retired"`)
}

func TestTaxHTTPFiltersAndPaging(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	party, _ := createTaxTestAccount(t, ts, map[string]any{"identifier": "synthetic12345"})
	resp, body := get(t, ts, "/api/v1/tax/accounts?filer="+party.PartyID+"&country=zz&taxkind=example", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.TaxAccountPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 100, page.Limit)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "****2345", page.Items[0].Identifier)
	resp, body = get(t, ts, "/api/v1/tax/accounts?offset=1", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, `"items":[]`)
	for _, query := range []string{"country=US", "taxkind=unmatched", "filer=22222222-2222-4222-8222-222222222222"} {
		resp, body = get(t, ts, "/api/v1/tax/accounts?"+query, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Contains(t, body, `"items":[]`)
	}
	resp, body = get(t, ts, "/api/v1/tax/accounts?filer=private-synthetic-value", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, "filer: invalid value")
	assert.NotContains(t, body, "private-synthetic-value")
	for _, query := range []string{"limit=0", "limit=1001", "offset=-1", "country=invalid", "country=%C5%BFZ"} {
		resp, body = get(t, ts, "/api/v1/tax/accounts?"+query, nil)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
}

func TestTaxHTTPAuditRefusalPreservesAccount(t *testing.T) {
	t.Parallel()
	ts, fixture := newTestServer(t, nil)
	party, account := createTaxTestAccount(t, ts, nil)
	input := map[string]any{"filer_party_id": party.PartyID, "country": "ZZ", "tax_kind": "Example", "cadence": "annual", "currency": "USD"}
	var resp *http.Response
	var body string
	plan, err := fixture.PreviewInitialAudit(t.Context(), fixture.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = fixture.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	path := "/api/v1/tax/accounts/" + account.AccountID
	for _, operation := range []struct {
		method, path string
		body         any
		headers      map[string]string
	}{
		{http.MethodPost, "/api/v1/tax/accounts", input, nil},
		{http.MethodPatch, path, map[string]any{"note": "example"}, map[string]string{"If-Match": `"1"`}},
		{http.MethodPost, path + "/retire", nil, map[string]string{"If-Match": `"1"`}},
	} {
		resp, body = do(t, ts, operation.method, operation.path, operation.headers, operation.body)
		assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
		assert.Contains(t, body, `"code":"audit_mutation_unsupported"`)
	}
	resp, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &account))
	assert.Equal(t, int64(1), account.Revision)
	assert.Nil(t, account.RetiredAt)
}

func TestTaxHTTPRequestLogsOmitValues(t *testing.T) {
	logs, err := os.CreateTemp(t.TempDir(), "tax-log-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = logs.Close() })
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	const sentinel = "private-synthetic-value"
	_, account := createTaxTestAccount(t, ts, map[string]any{"tax_kind": sentinel, "identifier": sentinel})
	_, _ = get(t, ts, "/api/v1/tax/accounts?taxkind="+sentinel, nil)
	_, _ = get(t, ts, "/api/v1/tax/accounts/"+account.AccountID+"?reveal=true", nil)
	_, _ = do(t, ts, http.MethodPatch, "/api/v1/tax/accounts/"+sentinel, map[string]string{"If-Match": sentinel}, map[string]any{"note": sentinel})
	content, err := os.ReadFile(logs.Name())
	require.NoError(t, err)
	assert.NotContains(t, string(content), sentinel)
	assert.NotContains(t, string(content), "Example Holdings Ltd.")
}
