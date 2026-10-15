package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoRoutesReadAssets(t *testing.T) {
	t.Parallel()
	ts, fixture := newTestServer(t, nil)
	hash, size, err := fixture.Blobs.Write(strings.NewReader("jpeg"))
	require.NoError(t, err)
	node, err := fixture.CreateFile(t.Context(), fixture.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := fixture.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	_, err = fixture.EditPhotoAuthoredBatch(t.Context(), store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{{AssetID: asset.ID, AssetRevision: asset.Revision, FileID: asset.Files[0].ID, Revision: asset.Files[0].Revision, NodeRevision: node.Revision}}, Patch: store.PhotoAuthoredPatch{Caption: new("")}})
	require.NoError(t, err)

	resp, body := get(t, ts, "/api/v1/photos/assets/"+asset.ID, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, strconv.Quote(strconv.FormatInt(asset.Revision, 10)), resp.Header.Get("ETag"))

	resp, body = get(t, ts, "/api/v1/photos/nodes/"+strconv.FormatInt(node.ID, 10)+"/asset", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	var byNode api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(body), &byNode))
	storedAsset, err := fixture.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, byNode.ID)
	assert.Equal(t, asset.Revision, byNode.Revision)
	require.Len(t, byNode.Files, len(storedAsset.Files))
	assert.Equal(t, store.PhotoConfirmedCaption, storedAsset.Files[0].Confirmed)
	assert.Equal(t, storedAsset.Files[0].Confirmed, byNode.Files[0].Confirmed)
}

func TestPhotoRoutesEnforceAssetAndTrashRevisions(t *testing.T) {
	t.Parallel()
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

	stale, staleBody := do(t, ts, http.MethodPost, path,
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(asset.Revision, 10))},
		map[string]bool{"excluded": true})
	assert.Equal(t, http.StatusPreconditionFailed, stale.StatusCode, staleBody)
	assert.Equal(t, "stale_revision", decodeProblem(t, staleBody).Code)
	path = "/api/v1/photos/assets/" + asset.ID + "/trash"
	resp, body = do(t, ts, http.MethodPost, path, map[string]string{"If-Match": "999"}, nil)
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, path, map[string]string{"If-Match": strconv.FormatInt(updated.Revision, 10)}, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	var receipt api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	stored, err := fixture.PhotoAssetByID(t.Context(), asset.ID)
	require.NoError(t, err)
	assert.Greater(t, stored.Revision, updated.Revision)
	assert.Equal(t, stored.Revision, receipt.Revision)
}

func TestPhotoRoutesCreateAndPromote(t *testing.T) {
	t.Parallel()
	ts, fixture := newTestServer(t, nil)
	hash, size, err := fixture.Blobs.Write(strings.NewReader("raw"))
	require.NoError(t, err)
	node, err := fixture.CreateFile(t.Context(), fixture.RootID(), "capture.bin", hash, size, "application/octet-stream")
	require.NoError(t, err)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets", nil,
		map[string]any{"node_id": node.ID, "role": "raw"})
	assert.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var created api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	inspected, err := fixture.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, inspected.Agreement, created.Agreement)
	assert.NotEmpty(t, created.CreatedAt)
	assert.NotEmpty(t, created.UpdatedAt)
	assert.Equal(t, strconv.Quote("1"), resp.Header.Get("ETag"))

	excluded, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/"+created.ID+"/exclude",
		map[string]string{"If-Match": strconv.Quote("1")}, map[string]bool{"excluded": true})
	assert.Equal(t, http.StatusOK, excluded.StatusCode, body)
	var excludedAsset api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(body), &excludedAsset))

	missing, body := do(t, ts, http.MethodPost, "/api/v1/photos/nodes/"+strconv.FormatInt(node.ID, 10)+"/promote", nil,
		map[string]string{"role": "raw"})
	assert.Equal(t, http.StatusPreconditionFailed, missing.StatusCode, body)
	assert.Equal(t, "stale_revision", decodeProblem(t, body).Code)

	promoted, body := do(t, ts, http.MethodPost, "/api/v1/photos/nodes/"+strconv.FormatInt(node.ID, 10)+"/promote",
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(excludedAsset.Revision, 10))},
		map[string]string{"role": "raw"})
	assert.Equal(t, http.StatusOK, promoted.StatusCode, body)
	var promotedAsset api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(body), &promotedAsset))
	assert.NotEmpty(t, promotedAsset.CreatedAt)
	assert.NotEmpty(t, promotedAsset.UpdatedAt)
	assert.Nil(t, promotedAsset.ExcludedAt)

	secondHash, secondSize, err := fixture.Blobs.Write(strings.NewReader("second raw"))
	require.NoError(t, err)
	secondNode, err := fixture.CreateFile(t.Context(), fixture.RootID(), "second.bin", secondHash, secondSize, "application/octet-stream")
	require.NoError(t, err)
	secondResp, secondBody := do(t, ts, http.MethodPost, "/api/v1/photos/nodes/"+strconv.FormatInt(secondNode.ID, 10)+"/promote", nil,
		map[string]any{"role": "raw"})
	assert.Equal(t, http.StatusOK, secondResp.StatusCode, secondBody)
	var secondAsset store.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(secondBody), &secondAsset))
	storedSecond, err := fixture.PhotoAssetForNode(t.Context(), secondNode.ID)
	require.NoError(t, err)
	assert.Equal(t, storedSecond, secondAsset)
}

func TestPhotoRoutesEnforceIfMatch(t *testing.T) {
	t.Parallel()
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

	stale, staleBody := do(t, ts, http.MethodPost, path,
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(asset.Revision, 10))},
		map[string]bool{"excluded": true})
	assert.Equal(t, http.StatusPreconditionFailed, stale.StatusCode, staleBody)
	assert.Equal(t, "stale_revision", decodeProblem(t, staleBody).Code)
}

func TestPhotoTrashRouteRevision(t *testing.T) {
	t.Parallel()
	ts, fixture := newTestServer(t, nil)
	hash, size, err := fixture.Blobs.Write(strings.NewReader("synthetic jpeg"))
	require.NoError(t, err)
	node, err := fixture.CreateFile(t.Context(), fixture.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := fixture.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	path := "/api/v1/photos/assets/" + asset.ID + "/trash"
	resp, body := do(t, ts, http.MethodPost, path, map[string]string{"If-Match": "999"}, nil)
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, path, map[string]string{"If-Match": strconv.FormatInt(asset.Revision, 10)}, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	var receipt api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
}

func TestPhotoRoutesCreatePromoteAndConcurrentRevisionWinner(t *testing.T) {
	t.Parallel()
	ts, fixture := newTestServer(t, nil)
	hash, size, err := fixture.Blobs.Write(strings.NewReader("raw"))
	require.NoError(t, err)
	node, err := fixture.CreateFile(t.Context(), fixture.RootID(), "capture.bin", hash, size, "application/octet-stream")
	require.NoError(t, err)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets", nil,
		map[string]any{"node_id": node.ID, "role": "raw"})
	assert.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var created api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	inspected, err := fixture.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, inspected.Agreement, created.Agreement)
	assert.NotEmpty(t, created.CreatedAt)
	assert.NotEmpty(t, created.UpdatedAt)
	assert.Equal(t, strconv.Quote("1"), resp.Header.Get("ETag"))

	excluded, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/"+created.ID+"/exclude",
		map[string]string{"If-Match": strconv.Quote("1")}, map[string]bool{"excluded": true})
	assert.Equal(t, http.StatusOK, excluded.StatusCode, body)
	var excludedAsset api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(body), &excludedAsset))

	missing, body := do(t, ts, http.MethodPost, "/api/v1/photos/nodes/"+strconv.FormatInt(node.ID, 10)+"/promote", nil,
		map[string]string{"role": "raw"})
	assert.Equal(t, http.StatusPreconditionFailed, missing.StatusCode, body)
	assert.Equal(t, "stale_revision", decodeProblem(t, body).Code)

	promoted, body := do(t, ts, http.MethodPost, "/api/v1/photos/nodes/"+strconv.FormatInt(node.ID, 10)+"/promote",
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(excludedAsset.Revision, 10))},
		map[string]string{"role": "raw"})
	assert.Equal(t, http.StatusOK, promoted.StatusCode, body)
	var promotedAsset api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(body), &promotedAsset))
	assert.NotEmpty(t, promotedAsset.CreatedAt)
	assert.NotEmpty(t, promotedAsset.UpdatedAt)
	assert.Nil(t, promotedAsset.ExcludedAt)

	secondHash, secondSize, err := fixture.Blobs.Write(strings.NewReader("second raw"))
	require.NoError(t, err)
	secondNode, err := fixture.CreateFile(t.Context(), fixture.RootID(), "second.bin", secondHash, secondSize, "application/octet-stream")
	require.NoError(t, err)
	secondResp, secondBody := do(t, ts, http.MethodPost, "/api/v1/photos/nodes/"+strconv.FormatInt(secondNode.ID, 10)+"/promote", nil,
		map[string]any{"role": "raw"})
	assert.Equal(t, http.StatusOK, secondResp.StatusCode, secondBody)
	var secondAsset api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(secondBody), &secondAsset))

	path := "/api/v1/photos/assets/" + secondAsset.ID + "/exclude"
	header := map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(secondAsset.Revision, 10))}
	var wait sync.WaitGroup
	results := make(chan int, 2)
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			response, _, requestErr := try(t, ts, http.MethodPost, path, header, map[string]bool{"excluded": true})
			if requestErr != nil {
				results <- 0
				return
			}
			results <- response.StatusCode
		}()
	}
	wait.Wait()
	close(results)
	statuses := []int{}
	for status := range results {
		statuses = append(statuses, status)
	}
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusPreconditionFailed}, statuses)
}
