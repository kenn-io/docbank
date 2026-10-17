package api_test

import (
	"encoding/json/v2"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoXMPResolveRoute(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	create := func(name, mime, data string) store.Node {
		hash, size, err := s.Blobs.Write(strings.NewReader(data))
		require.NoError(t, err)
		node, err := s.CreateFile(ctx, s.RootID(), name, hash, size, mime)
		require.NoError(t, err)
		return node
	}
	photo := create("photo.jpg", "image/jpeg", "synthetic JPEG")
	sidecar := create("photo.jpg.xmp", "application/rdf+xml", `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmp:Rating="1"/></rdf:RDF></x:xmpmeta>`)
	asset, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	file := asset.Files[0]
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, store.PhotoRoleSidecar, &file.ID)
	require.NoError(t, err)
	canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1})
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(ctx, photo.BlobHash, strings.Repeat("e", 64), canonical)
	require.NoError(t, err)
	photo, err = s.NodeByID(ctx, photo.ID)
	require.NoError(t, err)
	_, err = s.EditPhotoAuthoredBatch(ctx, store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{{AssetID: asset.ID, AssetRevision: asset.Revision, FileID: file.ID, Revision: file.Revision, NodeRevision: photo.Revision}}, Patch: store.PhotoAuthoredPatch{Rating: new(5)}})
	require.NoError(t, err)
	require.NoError(t, processing.SyncPhotoXMPBackground(ctx, &ingest.Ingester{Store: s.Store, Blobs: s.Blobs}, file.ID))
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/authored/prepare", nil, api.PhotoAuthoredPrepareRequest{AssetIDs: []string{asset.ID}})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var prepared api.PhotoAuthoredPreparation
	require.NoError(t, json.Unmarshal([]byte(body), &prepared))
	conflict := prepared.Targets[0].XMP
	require.Equal(t, store.PhotoXMPStateConflict, conflict.State)

	path := "/api/v1/photos/authored/" + file.ID + "/xmp"
	response, _ = do(t, ts, http.MethodPost, path, nil, api.PhotoXMPResolveRequest{ConflictID: "00000000-0000-4000-8000-000000000001", Choice: "catalog"})
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode)
	choice := api.PhotoXMPResolveRequest{ConflictID: conflict.ConflictID, Choice: "sidecar"}
	response, body = do(t, ts, http.MethodPost, path, nil, choice)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var resolved store.PhotoXMPState
	require.NoError(t, json.Unmarshal([]byte(body), &resolved))
	require.Equal(t, store.PhotoXMPStateSynced, resolved.State)
	current, err := s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	original := slices.IndexFunc(current.Files, func(f store.PhotoFile) bool { return f.ID == file.ID })
	require.Equal(t, 1, current.Files[original].Rating)
	response, _ = do(t, ts, http.MethodPost, path, nil, choice)
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode, "a resolved conflict cannot be chosen again")
}

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
		targets[i] = target.PhotoAuthoredEditTarget
	}
	return targets
}
