package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestTaxEventHTTPWorkflowAndReferenceDisclosure(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, account := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example"})
	path := "/api/v1/tax/records/" + r.RecordID
	secret := "synthetic-private-reference"
	resp, body := do(t, ts, http.MethodPost, path+"/advance?reveal=true", map[string]string{"If-Match": `"1"`}, map[string]any{"state": "filed", "occurred_on": "2026-01-01", "reference": secret})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, secret)
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))
	var receipt struct {
		Event struct {
			EventID string `json:"event_id"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	resp, body = get(t, ts, path, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, secret)
	assert.Contains(t, body, "****ence")
	etag := resp.Header.Get("ETag")
	resp, body = get(t, ts, path+"?reveal=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, secret)
	assert.Equal(t, etag, resp.Header.Get("ETag"))
	resp, body = do(t, ts, http.MethodPost, path+"/events/"+receipt.Event.EventID+"/void?reveal=true", map[string]string{"If-Match": etag}, map[string]any{"reason": "mistake"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, secret)
	resp, body = get(t, ts, path+"?reveal=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, secret)
	assert.Contains(t, body, `"state":"expected"`)
}

func TestTaxEventReferenceBoundariesAndWriteRejection(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, account := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example"})
	path := "/api/v1/tax/records/" + r.RecordID
	refs := []struct{ raw, masked string }{{"", ""}, {"a", "****"}, {"abcd", "****"}, {"abcde", "****bcde"}, {"一二三四五", "****二三四五"}}
	for i, ref := range refs {
		resp, body := do(t, ts, http.MethodPost, path+"/events", map[string]string{"If-Match": fmt.Sprintf(`"%d"`, i+1)}, map[string]any{"kind": "note", "occurred_on": "2026-01-01", "reference": ref.raw})
		require.Equal(t, http.StatusCreated, resp.StatusCode, body)
		if len([]rune(ref.raw)) > 4 {
			assert.NotContains(t, body, ref.raw)
		}
		assert.Contains(t, body, `"reference":"`+ref.masked+`"`)
	}
	resp, body := get(t, ts, path+"?reveal=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	for _, ref := range refs {
		assert.Contains(t, body, `"reference":"`+ref.raw+`"`)
	}
	resp, body = do(t, ts, http.MethodPost, path+"/events", map[string]string{"If-Match": `"6"`}, map[string]any{"kind": "note", "occurred_on": "2026-01-01", "sets_state": "filed"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
}

func TestTaxEventRevisionsAndRejection(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, account := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example"})
	path := "/api/v1/tax/records/" + r.RecordID
	secret := "synthetic-sensitive-value"
	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{path + "/advance", map[string]any{"state": "filed", "occurred_on": "2026-01-01"}},
		{path + "/events", map[string]any{"kind": "note", "occurred_on": "2026-01-01"}},
		{path + "/events/11111111-1111-4111-8111-111111111111/void", map[string]any{"reason": "example"}},
	} {
		resp, body := do(t, ts, http.MethodPost, tc.path, nil, tc.body)
		assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	}
	for _, tc := range []struct {
		header map[string]string
		body   map[string]any
		status int
	}{
		{nil, map[string]any{"kind": "note", "occurred_on": "2026-01-01", "reference": secret}, http.StatusPreconditionRequired},
		{map[string]string{"If-Match": `"9"`}, map[string]any{"kind": "note", "occurred_on": "2026-01-01", "reference": secret}, http.StatusPreconditionFailed},
		{map[string]string{"If-Match": `"1"`}, map[string]any{"kind": "payment", "occurred_on": "2026-01-01", "reference": secret}, http.StatusUnprocessableEntity},
	} {
		resp, body := do(t, ts, http.MethodPost, path+"/events?reveal=true", tc.header, tc.body)
		assert.Equal(t, tc.status, resp.StatusCode, body)
		assert.NotContains(t, body, secret)
	}
	resp, body := get(t, ts, path+"?reveal=false", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, secret)
	assert.Equal(t, `"1"`, resp.Header.Get("ETag"))
	resp, body = get(t, ts, path+"?reveal=invalid", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.NotContains(t, body, secret)
}

func TestTaxEventLongReferenceAndRetiredDetail(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, account := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example"})
	path := "/api/v1/tax/records/" + r.RecordID
	secret := strings.Repeat("s", 256)
	resp, body := do(t, ts, http.MethodPost, path+"/events?reveal=true", map[string]string{"If-Match": `"1"`}, map[string]any{"kind": "note", "occurred_on": "2026-01-01", "reference": secret})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	assert.NotContains(t, body, secret)
	resp, body = do(t, ts, http.MethodPost, path+"/retire?reveal=true", map[string]string{"If-Match": `"2"`}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, secret)
	resp, body = get(t, ts, "/api/v1/tax/records?include_retired=true&reveal=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, secret)
	resp, body = get(t, ts, path+"?reveal=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, secret)
	assert.Equal(t, `"3"`, resp.Header.Get("ETag"))
}

func TestTaxEventRequestAdmission(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, account := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example"})
	path := "/api/v1/tax/records/" + r.RecordID
	const secret = "private-synthetic-sentinel"
	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{"/events", map[string]any{"kind": "note", "occurred_on": "2026-01-01", "reference": nil}},
		{"/events", map[string]any{"kind": "note", "occurred_on": "2026-01-01", "note": nil}},
		{"/events", map[string]any{"kind": "note", "occurred_on": "2026-01-01", "currency": nil}},
		{"/events", map[string]any{"kind": "note", "occurred_on": "2026-01-01", "Reference": secret}},
		{"/advance", map[string]any{"state": "filed", "occurred_on": "2026-01-01", "Note": secret}},
		{"/advance", map[string]any{"state": "filed", "occurred_on": "2026-01-01", "reference": nil}},
		{"/events/11111111-1111-4111-8111-111111111111/void", map[string]any{"Reason": secret}},
		{"/events/11111111-1111-4111-8111-111111111111/void", map[string]any{"reason": nil}},
	} {
		resp, body := do(t, ts, http.MethodPost, path+tc.path, map[string]string{"If-Match": `"1"`}, tc.body)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.NotContains(t, body, secret)
	}
	resp, body := do(t, ts, http.MethodPost, path+"/events", map[string]string{"If-Match": `"1"`}, map[string]any{"kind": "note", "occurred_on": "2026-01-01", "reference": "example"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var out api.TaxEventMutation
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	resp, body = do(t, ts, http.MethodPost, path+"/advance", map[string]string{"If-Match": `"2"`}, map[string]any{"state": "filed", "occurred_on": "2026-01-01", "amount": nil})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, path+"/events/"+out.Event.EventID+"/void", map[string]string{"If-Match": `"3"`}, map[string]any{"reason": "Correction"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
}

func TestTaxEventInputDiagnostics(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	_, account := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example"})
	path := "/api/v1/tax/records/" + r.RecordID
	for _, tc := range []struct {
		path, field string
		body        map[string]any
	}{
		{"/events", "kind", map[string]any{"kind": "private-synthetic", "occurred_on": "2026-01-01"}},
		{"/advance", "state", map[string]any{"state": "private-synthetic", "occurred_on": "2026-01-01"}},
		{"/advance", "state", map[string]any{"state": "note", "occurred_on": "2026-01-01"}},
	} {
		resp, body := do(t, ts, http.MethodPost, path+tc.path, map[string]string{"If-Match": `"1"`}, tc.body)
		assert.Equal(t, 422, resp.StatusCode)
		assert.Contains(t, body, tc.field+": unsupported event")
		assert.NotContains(t, body, "private-synthetic")
	}
	for _, field := range []string{"amount", "currency"} {
		resp, body := do(t, ts, http.MethodPost, path+"/events", map[string]string{"If-Match": `"1"`}, map[string]any{"kind": "note", "occurred_on": "2026-01-01", field: "private-synthetic"})
		assert.Equal(t, 422, resp.StatusCode)
		assert.NotContains(t, body, "private-synthetic")
	}
}
