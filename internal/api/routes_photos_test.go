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
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

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

func TestPhotoRoutesAndClientTraversal(t *testing.T) {
	t.Parallel()
	ts, fixture := newTestServer(t, nil)
	hash, size, err := fixture.Blobs.Write(strings.NewReader("jpeg"))
	require.NoError(t, err)
	node, err := fixture.CreateFile(t.Context(), fixture.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := fixture.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	client := daemonconn.New(ts.URL, testAPIKey)
	_, err = fixture.EditPhotoAuthored(t.Context(), []store.PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: 1, Patch: store.PhotoAuthoredPatch{Caption: new("")}}})
	require.NoError(t, err)

	resp, body := get(t, ts, "/api/v1/photos/assets/"+asset.ID, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, strconv.Quote(strconv.FormatInt(asset.Revision, 10)), resp.Header.Get("ETag"))

	byNode, err := client.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, byNode.ID)
	assert.Equal(t, asset.Revision, byNode.Revision)
	assert.Equal(t, store.PhotoConfirmedCaption, byNode.Files[0].Confirmed)
}

func TestTrashEmptyReportsPhotoHeldRoots(t *testing.T) {
	t.Parallel()
	ts, fixture := newTestServer(t, nil)
	rawHash, rawSize, err := fixture.Blobs.Write(strings.NewReader("synthetic raw"))
	require.NoError(t, err)
	raw, err := fixture.CreateFile(t.Context(), fixture.RootID(), "capture.raw", rawHash, rawSize, "application/octet-stream")
	require.NoError(t, err)
	jpegHash, jpegSize, err := fixture.Blobs.Write(strings.NewReader("synthetic jpeg"))
	require.NoError(t, err)
	jpeg, err := fixture.CreateFile(t.Context(), fixture.RootID(), "capture.jpg", jpegHash, jpegSize, "image/jpeg")
	require.NoError(t, err)
	auto, err := fixture.PhotoAssetForNode(t.Context(), jpeg.ID)
	require.NoError(t, err)
	_, err = fixture.DetachPhotoFile(t.Context(), auto.ID, auto.Revision, auto.Files[0].ID, store.PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := fixture.CreatePhotoAsset(t.Context(), raw.ID, store.PhotoRoleRAW, store.PhotoKindPhoto)
	require.NoError(t, err)
	_, err = fixture.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, jpeg.ID, store.PhotoRoleImage, nil)
	require.NoError(t, err)
	_, _, err = fixture.Trash(t.Context(), raw.ID, store.UnconditionalRev)
	require.NoError(t, err)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil, map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.TrashEmptyReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	assert.Equal(t, api.TrashEmptyReport{HeldRoots: 1, Run: true}, report)
}
