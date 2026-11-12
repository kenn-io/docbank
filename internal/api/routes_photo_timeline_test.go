package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestPhotoTimelineRouteCountsBrowsedPhotos(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	for _, name := range []string{"a.jpg", "b.jpg", "notes.txt"} {
		mime := "image/jpeg"
		if name == "notes.txt" {
			mime = "text/plain"
		}
		_, err := s.CreateFile(t.Context(), s.RootID(), name, testHash(name), 10, mime)
		require.NoError(t, err)
	}
	request := api.PhotoTimelineRequest{Query: api.QueryPayload(`{"sort":{"field":"capture_time","direction":"desc"}}`)}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/timeline", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var timeline api.PhotoTimeline
	require.NoError(t, json.Unmarshal([]byte(body), &timeline))
	require.Len(t, timeline.Facets, 1)
	facet := timeline.Facets[0]
	assert.Equal(t, "capture_day", facet.Dimension)
	require.True(t, facet.Available)
	assert.Equal(t, int64(2), *facet.Total)
	assert.Equal(t, int64(2), *facet.Missing)
	assert.Empty(t, facet.Values)
	for _, field := range []string{"snapshot_id", "member_hash", "rows", "next_cursor", "facets_only"} {
		assert.NotContains(t, body, `"`+field+`"`)
	}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: request.Query})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, page.Total, *facet.Total)
}

func TestPhotoTimelineRouteRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	headers := map[string]string{"X-Api-Key": testAPIKey}
	for _, raw := range []string{
		`{"query":{"syntax":"advanced","text":"tag:"}}`,
		`{"query":{},"coverage":{"configuration":"configured"}}`,
		`{"query":{},"facets":["capture_day"]}`,
	} {
		response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/photos/assets/timeline", headers, raw)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, raw+": "+body)
	}
	for _, raw := range []string{
		`{"query":{},"facets":["capture_day"]}`,
		`{"query":{},"facets_only":true}`,
	} {
		response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", headers, raw)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, raw+": "+body)
	}
	saved, _ := createSavedQuery(t, ts.URL, "Calendar rejected for saved runs", `{}`)
	response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries/"+saved.ID+"/runs",
		map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}, `{"facets":["capture_day"]}`)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
}

func TestPhotoTimelineRouteAcceptsBrowserSessions(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	request := api.PhotoTimelineRequest{Query: api.QueryPayload(`{}`)}
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/timeline", headers, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/timeline?unexpected=1", headers, request)
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
}
