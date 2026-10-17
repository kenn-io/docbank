package api_test

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
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
	request := api.PhotoStackDecisionRequest{BurstGapSeconds: 30, Hidden: true, AssetIDs: ids, Targets: targets, Decision: "representative", CurrentID: ids[0], SelectedIDs: ids}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	require.Greater(t, len(encoded), 512<<10)
	require.LessOrEqual(t, len(encoded), 576<<10)
	res, body := do(t, ts, http.MethodPost, "/api/v1/photos/authored/stack", nil, request)
	require.Equal(t, http.StatusForbidden, res.StatusCode, body)
	require.Contains(t, body, "hidden_locked")
	res, body = do(t, ts, http.MethodPost, "/api/v1/photos/authored/stack", nil, map[string]any{"padding": strings.Repeat("x", 1<<20)})
	require.Equal(t, http.StatusRequestEntityTooLarge, res.StatusCode, body)
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
