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
	for i, state := range []document.VisualPreviewState{document.VisualPreviewUnsupported, document.VisualPreviewFailed} {
		terminal, err := s.CreateFile(t.Context(), s.RootID(), string(state)+".jpg", testHash(string(state)), 10, "image/jpeg")
		require.NoError(t, err)
		recipe, err := document.BuiltInVisualPreviewRecipe("grid")
		require.NoError(t, err)
		canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{
			ContractVersion: document.VisualPreviewContractV1, SourceSHA256: terminal.BlobHash, Recipe: recipe, State: state,
			Failure: &document.VisualPreviewFailureV1{Code: "decode_failed", Detail: "synthetic terminal preview"},
		})
		require.NoError(t, err)
		_, err = s.PublishVisualPreview(t.Context(), terminal.CurrentVersionID, canonical, nil)
		require.NoError(t, err)
		request.Query = api.QueryPayload(`{"filters":{"unevaluated":true}}`)
		response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		for _, row := range page.Items {
			require.Equal(t, "unavailable", row.Quality.State)
			require.Nil(t, row.Quality.Signals)
		}
		require.Len(t, page.Items, i+1)
		require.Contains(t, body, `"signals":null`)
	}
	_, err = s.CreateFile(t.Context(), s.RootID(), "quality.mp4", testHash("quality-video-api"), 10, "video/mp4")
	require.NoError(t, err)
	request.Query = api.QueryPayload(`{"filters":{"kinds":["video"]}}`)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NotContains(t, body, `"quality"`)
}
