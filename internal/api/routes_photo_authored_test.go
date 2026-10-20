package api_test

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoAuthoredRoutes(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	hash, size, err := s.Blobs.Write(strings.NewReader("synthetic JPEG"))
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, s.RootID(), "synthetic.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	preparedResponse, preparedBody := do(t, ts, http.MethodPost, "/api/v1/photos/authored/prepare", nil, api.PhotoAuthoredPrepareRequest{AssetIDs: []string{asset.ID}})
	require.Equal(t, http.StatusOK, preparedResponse.StatusCode, preparedBody)
	var preparation api.PhotoAuthoredPreparation
	require.NoError(t, json.Unmarshal([]byte(preparedBody), &preparation))
	invalidResponse, invalidBody := do(t, ts, http.MethodPost, "/api/v1/photos/authored/edit", nil, api.PhotoAuthoredEditRequest{Targets: apiEditTargets(preparation.Targets), Patch: store.PhotoAuthoredPatch{Rating: new(6)}})
	require.Equal(t, http.StatusUnprocessableEntity, invalidResponse.StatusCode, invalidBody)
	require.Contains(t, invalidBody, "body.patch.rating")
	editResponse, editBody := do(t, ts, http.MethodPost, "/api/v1/photos/authored/edit", nil, api.PhotoAuthoredEditRequest{Targets: apiEditTargets(preparation.Targets), RotationDelta: new(90)})
	require.Equal(t, http.StatusOK, editResponse.StatusCode, editBody)
	updated, err := daemonconn.New(ts.URL, testAPIKey).PhotoAsset(ctx, asset.ID)
	require.NoError(t, err)
	require.Equal(t, 90, updated.Files[0].Rotation)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, hideErr := s.SetPhotoAssetHidden(ctx, asset.ID, updated.Revision, true)
	require.NoError(t, hideErr)
	scope := api.PhotoAuthoredPrepareRequest{AssetIDs: []string{asset.ID}, Hidden: true}
	deniedResponse, deniedBody := do(t, ts, http.MethodPost, "/api/v1/photos/authored/prepare", nil, scope)
	require.Equal(t, http.StatusForbidden, deniedResponse.StatusCode, deniedBody)
	require.Contains(t, deniedBody, "hidden_locked")
	_, cookie, cookieErr := daemonconn.New(ts.URL, testAPIKey).PhotoHidden(ctx, "unlock", "synthetic-passcode", "")
	require.NoError(t, cookieErr)
	headers := map[string]string{"Cookie": cookie}
	allowedResponse, allowedBody := do(t, ts, http.MethodPost, "/api/v1/photos/authored/prepare", headers, scope)
	require.Equal(t, http.StatusOK, allowedResponse.StatusCode, allowedBody)
}

func apiEditTargets(prepared []store.PhotoPreparedTarget) []store.PhotoAuthoredEditTarget {
	targets := make([]store.PhotoAuthoredEditTarget, len(prepared))
	for i, target := range prepared {
		targets[i] = store.PhotoAuthoredEditTarget{PhotoAuthoredTargetRevision: target.PhotoAuthoredTargetRevision}
	}
	return targets
}

func TestPhotoStackDecisionValidatesSiblingIDs(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	id := "40000000-0000-4000-8000-000000000001"
	for _, sibling := range []string{"malformed", "40000000-0000-4000-8000-000000000002"} {
		request := api.PhotoStackDecisionRequest{BurstGapSeconds: 2, AssetIDs: []string{id, sibling}, CurrentID: id, Decision: "all", Targets: []store.PhotoAuthoredEditTarget{{AssetID: id, AssetRevision: 1, FileID: id, Revision: 1, NodeRevision: 1}}}
		res, body := do(t, ts, http.MethodPost, "/api/v1/photos/authored/stack", nil, request)
		if sibling == "malformed" {
			require.Equal(t, http.StatusUnprocessableEntity, res.StatusCode, body)
		} else {
			require.Equal(t, http.StatusConflict, res.StatusCode, body)
			require.Contains(t, body, "photo_stack_changed")
		}
	}
}

func TestPhotoStackDecisionBounds(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	ids := make([]string, 500)
	targets := make([]store.PhotoAuthoredEditTarget, 2000)
	for i := range ids {
		ids[i] = fmt.Sprintf("40000000-0000-4000-8000-%012d", i)
	}
	for i := range targets {
		targets[i] = store.PhotoAuthoredEditTarget{AssetID: ids[i/4], AssetRevision: math.MaxInt64, FileID: fmt.Sprintf("50000000-0000-4000-8000-%012d", i), Revision: math.MaxInt64, NodeRevision: math.MaxInt64, ContentVersionID: fmt.Sprintf("60000000-0000-4000-8000-%012d", i)}
	}
	request := api.PhotoStackDecisionRequest{BurstGapSeconds: 30, AssetIDs: ids, Targets: targets, Decision: "representative", CurrentID: ids[0], SelectedIDs: ids}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	// A stale observed stack confirms the complete under-limit request reaches
	// the store without creating 2,000 live targets.
	for _, tc := range []struct{ size, status int }{{(576 << 10) - 1, http.StatusConflict}, {576 << 10, http.StatusRequestEntityTooLarge}} {
		payload := string(encoded) + strings.Repeat(" ", tc.size-len(encoded))
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/photos/authored/stack", strings.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		res, err := ts.Client().Do(req)
		require.NoError(t, err)
		require.Equal(t, tc.status, res.StatusCode)
		require.NoError(t, res.Body.Close())
	}
}

func TestPhotoAuthoredRouteRefusesStaleContentVersion(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "synthetic.jpg", testHash("original-photo"), 10, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, testHash("replacement-photo"), 20, "image/jpeg")
	require.NoError(t, err)

	// Use fresh metadata revisions so only the viewed content binding is stale.
	resp, body := do(t, ts, http.MethodPost, "/api/v1/photos/authored/prepare", nil, api.PhotoAuthoredPrepareRequest{AssetIDs: []string{asset.ID}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var prepared api.PhotoAuthoredPreparation
	require.NoError(t, json.Unmarshal([]byte(body), &prepared))
	targets := apiEditTargets(prepared.Targets)
	require.Len(t, targets, 1)
	targets[0].ContentVersionID = node.CurrentVersionID
	request := api.PhotoAuthoredEditRequest{Targets: targets, Patch: store.PhotoAuthoredPatch{Flag: new("pick")}}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/photos/authored/edit", nil, request)
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	require.Equal(t, "stale_revision", decodeProblem(t, body).Code)
}

func TestPhotoAuthoredReceiptsRetainFlagOwner(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "synthetic receipt projection")
	require.NoError(t, err)
	var ids []string
	for i := range 2 {
		name := fmt.Sprintf("frame-%d.jpg", i)
		node, err := s.IngestFileExact(ctx, run, s.RootID(), name, testHash(name), 20, "image/jpeg", "/shots/"+name, "2024-06-10T12:00:00Z")
		require.NoError(t, err)
		stamp := fmt.Sprintf("2024-06-10T12:00:0%dZ", i)
		canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{{Key: "created", Namespace: "image.exif", SourceField: "DateTimeOriginal", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp, Timestamp: &document.SourceMetadataTimestampV1{Raw: stamp, Normalized: stamp, Precision: document.SourceMetadataPrecisionSecond, Timezone: document.SourceMetadataTimezoneUTC}}}}})
		require.NoError(t, err)
		_, err = s.PublishSourceMetadata(ctx, node.BlobHash, testHash("extractor"), canonical)
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		ids = append(ids, asset.ID)
	}
	assertGrouping := func(incomplete bool) {
		t.Helper()
		response, body := do(t, ts, http.MethodPost, "/api/v1/photos/stacks/group", nil, api.GroupPhotoStacksRequest{AssetIDs: ids})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var grouped api.GroupPhotoStacksResponse
		require.NoError(t, json.Unmarshal([]byte(body), &grouped))
		require.Equal(t, incomplete, grouped.GroupingIncomplete)
		require.Len(t, grouped.Groups, map[bool]int{true: 0, false: 1}[incomplete])
		response, body = do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, map[string]any{"query": map[string]any{"sort": map[string]string{"field": "capture_time", "direction": "asc"}}, "stacks": true})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var page api.PhotoBrowsePage
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.Equal(t, incomplete, page.GroupingIncomplete)
		require.Len(t, page.Items, 2)
		require.Len(t, page.Stacks, map[bool]int{true: 0, false: 1}[incomplete])
	}
	assertGrouping(true)
	for _, id := range ids {
		asset, err := s.PhotoAssetByID(ctx, id)
		require.NoError(t, err)
		node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
		require.NoError(t, err)
		generation, _, err := s.ActiveSourceMetadata(ctx, node.BlobHash)
		require.NoError(t, err)
		_, err = s.PublishSourceMetadata(ctx, node.BlobHash, processing.SourceMetadataExtractorFingerprint, generation.CanonicalJSON)
		require.NoError(t, err)
	}
	assertGrouping(false)
	prepared, err := s.PreparePhotoAuthored(ctx, store.PhotoSetSelection{AssetIDs: ids})
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/authored/stack", nil, api.PhotoStackDecisionRequest{BurstGapSeconds: 2, AssetIDs: ids, Targets: apiEditTargets(prepared.Targets), Decision: "current", CurrentID: ids[0]})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"flag_owner":""`)
	var receipt store.PhotoAuthoredReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	require.Len(t, receipt.After, 2)
	require.Equal(t, "return", receipt.After[0].Values.FlagOwner)
	// Undo retains ownership so another Return can replace these flags.
	undo, err := s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	require.NotEmpty(t, undo.Before[0].Values.FlagOwner)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/authored/"+undo.ReceiptID+"/undo", nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"flag_owner":"return"`)
	prepared, err = s.PreparePhotoAuthored(ctx, store.PhotoSetSelection{AssetIDs: ids})
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/authored/edit", nil, api.PhotoAuthoredEditRequest{Targets: apiEditTargets(prepared.Targets), Patch: store.PhotoAuthoredPatch{Rating: new(4)}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"flag_owner":"return"`)
	require.Contains(t, body, `"rating":4`)
}
