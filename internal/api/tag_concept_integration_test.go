package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestTagConceptHTTPAliasAndRevision(t *testing.T) {
	ts, catalog := newTestServer(t, nil)
	tag, err := catalog.CreateTag(t.Context(), "Café")
	require.NoError(t, err)
	conceptPath := "/api/v1/tags/" + tag.ID + "/concept"
	response, body := get(t, ts, conceptPath, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, `"1"`, response.Header.Get("ETag"))

	response, body = do(t, ts, http.MethodPut, conceptPath, nil,
		map[string]any{"description": "Synthetic topic"})
	require.Equal(t, http.StatusPreconditionRequired, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPut, conceptPath,
		map[string]string{"If-Match": `"1"`}, map[string]any{"description": "Synthetic topic"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, `"2"`, response.Header.Get("ETag"))

	response, body = do(t, ts, http.MethodPost, "/api/v1/tags/"+tag.ID+"/aliases",
		map[string]string{"If-Match": `"2"`}, map[string]any{"alias": "Coffee"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Equal(t, `"3"`, response.Header.Get("ETag"))

	response, body = get(t, ts, "/api/v1/tags/resolve-concept?name="+url.QueryEscape("Coffee"), nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var resolved api.Tag
	require.NoError(t, json.Unmarshal([]byte(body), &resolved))
	require.Equal(t, tag.ID, resolved.ID)
}
