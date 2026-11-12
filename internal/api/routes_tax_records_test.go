package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
