package api_test

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
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

func TestTaxEventValuesAbsentFromLogsAndErrors(t *testing.T) {
	logFile, err := os.CreateTemp(t.TempDir(), "tax-event-log-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = logFile.Close() })
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Logger = slog.New(slog.NewTextHandler(logFile, nil)) })
	_, account := createTaxTestAccount(t, ts, nil)
	r := createTaxTestRecord(t, ts, map[string]any{"account_id": account.AccountID, "kind": "filing", "title": "Example"})
	path := "/api/v1/tax/records/" + r.RecordID
	const secret = "synthetic-sensitive-sentinel"
	for _, field := range []string{"kind", "occurred_on", "currency", "reference", "note", "amount", "sets_state", "record_revision"} {
		resp, body := do(t, ts, http.MethodPost, path+"/events?reveal=true", map[string]string{"If-Match": `"1"`}, map[string]any{field: []any{secret}})
		assert.GreaterOrEqual(t, resp.StatusCode, 400)
		assert.NotContains(t, body, secret)
	}
	resp, body := do(t, ts, http.MethodPost, path+"/events/"+secret+"/void?reveal="+secret, map[string]string{"If-Match": secret}, map[string]any{"reason": secret})
	assert.GreaterOrEqual(t, resp.StatusCode, 400)
	assert.NotContains(t, body, secret)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+path+"/events?reveal=true", strings.NewReader(`{"reference":"`+secret))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"1"`)
	bad, err := ts.Client().Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(bad.Body)
	require.NoError(t, err)
	require.NoError(t, bad.Body.Close())
	assert.GreaterOrEqual(t, bad.StatusCode, 400)
	assert.NotContains(t, string(raw), secret)
	content, err := os.ReadFile(logFile.Name())
	require.NoError(t, err)
	assert.NotContains(t, string(content), secret)
}
