package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPhotoRoutesEnforceIfMatch(t *testing.T) {
	ts, fixture := newTestServer(t, nil)
	hash, size, err := fixture.Blobs.Write(strings.NewReader("jpeg"))
	require.NoError(t, err)
	node, err := fixture.CreateFile(t.Context(), fixture.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := fixture.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)

	path := "/api/v1/photos/assets/" + asset.ID + "/exclude"
	resp, body := do(t, ts, http.MethodPost, path, nil, map[string]bool{"excluded": true})
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	assert.Equal(t, "precondition_required", decodeProblem(t, body).Code)

	resp, body = do(t, ts, http.MethodPost, path,
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(asset.Revision, 10))},
		map[string]bool{"excluded": true})
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, strconv.Quote(strconv.FormatInt(asset.Revision+1, 10)), resp.Header.Get("ETag"))
	var updated api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(body), &updated))
	assert.Equal(t, asset.ID, updated.ID)
	assert.NotNil(t, updated.ExcludedAt)
}

func TestPhotoRoutesAndClientTraversal(t *testing.T) {
	ts, fixture := newTestServer(t, nil)
	hash, size, err := fixture.Blobs.Write(strings.NewReader("jpeg"))
	require.NoError(t, err)
	node, err := fixture.CreateFile(t.Context(), fixture.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := fixture.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)

	resp, body := get(t, ts, "/api/v1/photos/assets/"+asset.ID, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, strconv.Quote(strconv.FormatInt(asset.Revision, 10)), resp.Header.Get("ETag"))

	client := daemonconn.New(ts.URL, testAPIKey)
	byNode, err := client.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, byNode.ID)
	assert.Equal(t, asset.Revision, byNode.Revision)
}
