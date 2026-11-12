package api_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestTaxAccountHTTPWorkflow(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "Example Holdings Ltd.", "own": true})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/accounts", nil, map[string]any{"filer_party_id": party.PartyID, "country": "ZZ", "tax_kind": "Example", "cadence": "annual", "currency": "USD", "identifier": "synthetic12345", "effective_start": "2025-01-01"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var account api.TaxAccount
	require.NoError(t, json.Unmarshal([]byte(body), &account))
	assert.Equal(t, int64(14), account.LeadDays)
	assert.Equal(t, "****2345", account.Identifier)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	path := "/api/v1/tax/accounts/" + account.AccountID
	resp, body = get(t, ts, path+"?reveal=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &account))
	assert.Equal(t, "synthetic12345", account.Identifier)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, map[string]any{"effective_start": nil, "lead_days": 0})
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
	assert.Contains(t, body, `"items":[]`)
}

func TestTaxHTTPValidationRedaction(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	const sentinel = "private-synthetic-value"
	for _, input := range []any{
		map[string]any{"identifier": sentinel, sentinel: sentinel},
		map[string]any{"identifier": sentinel, "lead_days": sentinel},
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

func TestTaxHTTPRevisionGateAndImmutableFields(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "Example Holdings Ltd."})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/accounts", nil, map[string]any{"filer_party_id": party.PartyID, "country": "ZZ", "tax_kind": "Example", "cadence": "annual", "currency": "USD"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var account api.TaxAccount
	require.NoError(t, json.Unmarshal([]byte(body), &account))
	path := "/api/v1/tax/accounts/" + account.AccountID
	resp, body = do(t, ts, http.MethodPatch, path, nil, map[string]any{"note": "example"})
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"2"`}, map[string]any{"note": "example"})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	for _, patch := range []map[string]any{{"country": "ZZ"}, {"state": "private"}, {"revision": 2}, {"identifier": nil}, {"lead_days": nil}} {
		resp, body = do(t, ts, http.MethodPatch, path, map[string]string{"If-Match": `"1"`}, patch)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
	resp, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &account))
	assert.Equal(t, int64(1), account.Revision)
}

func TestTaxHTTPFiltersAndPaging(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "Example Holdings Ltd."})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/accounts", nil, map[string]any{"filer_party_id": party.PartyID, "country": "ZZ", "tax_kind": "Example", "cadence": "annual", "currency": "USD", "identifier": "synthetic12345"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/tax/accounts?filer="+party.PartyID+"&country=ZZ&taxkind=example", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.TaxAccountPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 100, page.Limit)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "****2345", page.Items[0].Identifier)
	resp, body = get(t, ts, "/api/v1/tax/accounts?offset=1", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, `"items":[]`)
	for _, query := range []string{"limit=0", "limit=1001", "offset=-1", "country=invalid"} {
		resp, body = get(t, ts, "/api/v1/tax/accounts?"+query, nil)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
}

func TestTaxHTTPAuditRefusalPreservesAccount(t *testing.T) {
	t.Parallel()
	ts, fixture := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/parties", nil, map[string]any{"kind": "organization", "display_name": "Example Holdings Ltd."})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(body), &party))
	input := map[string]any{"filer_party_id": party.PartyID, "country": "ZZ", "tax_kind": "Example", "cadence": "annual", "currency": "USD"}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/tax/accounts", nil, input)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var account api.TaxAccount
	require.NoError(t, json.Unmarshal([]byte(body), &account))
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
