package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
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

	resp, body := get(t, ts, "/api/v1/photos/assets/"+asset.ID, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, strconv.Quote(strconv.FormatInt(asset.Revision, 10)), resp.Header.Get("ETag"))

	byNode, err := client.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, byNode.ID)
	assert.Equal(t, asset.Revision, byNode.Revision)
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

func TestPhotoSettingsGapOnlyPreservesDisplayPreference(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "burst.cr2", testHash("burst-raw"), 10, "application/octet-stream")
	require.NoError(t, err)
	image, err := s.CreateFile(ctx, s.RootID(), "burst.jpg", testHash("burst-jpeg"), 10, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, image.ID)
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, raw.ID, store.PhotoRoleRAW, nil)
	require.NoError(t, err)
	res, body := do(t, ts, http.MethodPut, "/api/v1/photos/settings", map[string]string{"If-Match": `"1"`}, map[string]any{"preference": "image"})
	require.Equal(t, 200, res.StatusCode, body)
	before, err := s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts), "If-Match": `"2"`}
	res, body = do(t, ts, http.MethodPut, "/api/v1/photos/settings", browser, map[string]any{"burst_gap_seconds": 5})
	require.Equal(t, 200, res.StatusCode, body)
	var settings api.PhotoSettings
	require.NoError(t, json.Unmarshal([]byte(body), &settings))
	require.Equal(t, new("image"), settings.Preference)
	require.Equal(t, 5, settings.BurstGapSeconds)
	after, err := s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	require.Equal(t, before.DisplayFileID, after.DisplayFileID)
	require.Equal(t, before.Revision, after.Revision)
	res, body = do(t, ts, http.MethodPut, "/api/v1/photos/settings", map[string]string{"If-Match": `"3"`}, map[string]any{"burst_gap_seconds": 5})
	require.Equal(t, 200, res.StatusCode, body)
	require.Equal(t, `"3"`, res.Header.Get("ETag"))
	browser["If-Match"] = `"3"`
	res, body = do(t, ts, http.MethodPut, "/api/v1/photos/settings", browser, map[string]any{"preference": "raw"})
	require.Equal(t, http.StatusForbidden, res.StatusCode, body)
	res, body = do(t, ts, http.MethodPut, "/api/v1/photos/settings", map[string]string{"If-Match": `"3"`}, map[string]any{"preference": nil})
	require.Equal(t, 200, res.StatusCode, body)
	after, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	require.NotEqual(t, before.DisplayFileID, after.DisplayFileID)
}

func TestPhotoStackGroupRouteBoundsExplicitIDs(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "synthetic burst")
	require.NoError(t, err)
	ids := make([]string, 3)
	for i, source := range []string{"/roll/one.jpg", "/roll/two.jpg", "/other/three.jpg"} {
		node, err := s.IngestFileExact(ctx, run, s.RootID(), strconv.Itoa(i)+".jpg", testHash(source), 10, "image/jpeg", source, "2024-01-01T00:00:00Z")
		require.NoError(t, err)
		stamp := "2024-06-10T12:00:0" + strconv.Itoa(i) + "Z"
		canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{{
			Key: "created", Namespace: "image.exif", SourceField: "DateTimeOriginal",
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp, Timestamp: &document.SourceMetadataTimestampV1{
				Raw: stamp, Normalized: stamp, Precision: document.SourceMetadataPrecisionSecond, Timezone: document.SourceMetadataTimezoneUTC,
			}},
		}}})
		require.NoError(t, err)
		_, err = s.PublishSourceMetadata(ctx, node.BlobHash, testHash("synthetic metadata"), canonical)
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		ids[i] = asset.ID
	}
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	missing := "00000000-0000-4000-8000-000000000009"
	res, body := do(t, ts, http.MethodPost, "/api/v1/photos/stacks/group", browser, map[string]any{"asset_ids": []string{ids[1], missing, ids[2], ids[0]}})
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	var grouped api.GroupPhotoStacksResponse
	require.NoError(t, json.Unmarshal([]byte(body), &grouped))
	require.Equal(t, api.GroupPhotoStacksResponse{BurstGapSeconds: 2, Groups: [][]string{{ids[1], ids[0]}}, UnavailableIDs: []string{missing}}, grouped)

	// A full Cull run fits the request bound, and so does its flat response.
	run250k := make([]string, 250_000)
	for i := range run250k {
		run250k[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	res, body = do(t, ts, http.MethodPost, "/api/v1/photos/stacks/group", browser, map[string]any{"asset_ids": run250k})
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Less(t, len(body), 16<<20)
	grouped = api.GroupPhotoStacksResponse{}
	require.NoError(t, json.Unmarshal([]byte(body), &grouped))
	require.Empty(t, grouped.Groups)
	require.Len(t, grouped.UnavailableIDs, 250_000)
	// Repeats are rejected without echoing the run back once per repeat.
	res, body = do(t, ts, http.MethodPost, "/api/v1/photos/stacks/group", browser, map[string]any{"asset_ids": slices.Repeat([]string{ids[0]}, 250_000)})
	require.Equal(t, http.StatusUnprocessableEntity, res.StatusCode)
	require.Less(t, len(body), 64<<10)
	res, _ = do(t, ts, http.MethodPost, "/api/v1/photos/stacks/group", browser, map[string]any{"asset_ids": slices.Concat(run250k, []string{missing})})
	require.Equal(t, http.StatusUnprocessableEntity, res.StatusCode)
	res, _ = do(t, ts, http.MethodPost, "/api/v1/photos/stacks/group", browser, map[string]any{"asset_ids": []string{}, "padding": strings.Repeat("x", 16<<20)})
	require.Equal(t, http.StatusRequestEntityTooLarge, res.StatusCode)
}
