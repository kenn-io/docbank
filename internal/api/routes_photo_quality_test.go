package api_test

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"net/http"
	"testing"
)

func TestPhotoQualityBrowseContract(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	node, err := s.CreateFile(t.Context(), s.RootID(), "quality.jpg", testHash("quality-api"), 10, "image/jpeg")
	require.NoError(t, err)
	request := api.PhotoBrowseRequest{Query: api.QueryPayload(`{"filters":{"kinds":["photo"]}}`)}
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.PhotoBrowsePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, "pending", page.Items[0].Quality.State)
	require.Contains(t, body, `"signals":null`)
	require.NoError(t, s.PublishPhotoQualitySignals(t.Context(), store.PhotoVisualPreviewTarget{VersionID: node.CurrentVersionID, SourceSHA256: node.BlobHash, Size: 10, MediaType: "image/jpeg"}, document.PhotoQualitySignals{Blur: 1}))
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, "ready", page.Items[0].Quality.State)
	require.Contains(t, body, `"focus":0`)
	require.Zero(t, page.Items[0].Quality.Signals.Focus)
	request.Query = api.QueryPayload(`{"filters":{"focus_max":"0","blur_min":"1"}}`)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	for _, raw := range []string{`{"filters":{"focus_min":0.7}}`, `{"filters":{"focus_min":"1.1"}}`, `{"filters":{"focus_min":"0.8","focus_max":"0.7"}}`} {
		request.Query = api.QueryPayload(raw)
		response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	}
	_, err = s.CreateFile(t.Context(), s.RootID(), "quality.mp4", testHash("quality-video-api"), 10, "video/mp4")
	require.NoError(t, err)
	request.Query = api.QueryPayload(`{"filters":{"kinds":["video"]}}`)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NotContains(t, body, `"quality"`)
}
