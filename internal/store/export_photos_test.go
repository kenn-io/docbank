package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"uuid"
)

func TestPhotoExportPlanSealsFrozenInputsAndOwnsArtifact(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newTestStore(t)
	n := browsePhotoNode(t, s, "render.jpg", fakeHash("a3"), "image/jpeg")
	m := bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}
	source, err := s.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{m}}, nil)
	require.NoError(t, err)
	r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "jpeg", Quality: 90, IncludeMetadata: true, RemoveGPS: true}}
	inputs, err := s.ExportPhotoInputs(ctx, "owner", r)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	a := PreparedPhotoExport{Input: inputs[0], Receipt: bundle.PhotoRenderReceipt{Version: bundle.PhotoRenderReceiptVersion, Profile: *r.PhotoRender, Source: m, Width: 10, Height: 20}, SHA256: fakeHash("b3"), Size: 99, Physical: BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 99, Created: true}}
	asset, err := s.PhotoAssetForNode(ctx, n.ID)
	require.NoError(t, err)
	rating := 4
	_, err = s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: asset.Files[0].Revision, Patch: PhotoAuthoredPatch{Rating: &rating}}})
	require.NoError(t, err)
	_, err = s.SealPhotoExportPlan(ctx, "owner", r, []PreparedPhotoExport{a})
	require.ErrorIs(t, err, bundle.ErrConflict)
	var roots int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM export_role_roots`).Scan(&roots))
	require.Zero(t, roots)
	inputs, err = s.ExportPhotoInputs(ctx, "owner", r)
	require.NoError(t, err)
	a.Input = inputs[0]
	plan, err := s.SealPhotoExportPlan(ctx, "owner", r, []PreparedPhotoExport{a})
	require.NoError(t, err)
	preview, err := s.ExportPlanPreview(ctx, "owner", plan.ID)
	require.NoError(t, err)
	require.Equal(t, int64(99), preview.Roles[0].Bytes)
	replay, found, err := s.ExportPlanReplay(ctx, "owner", r)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, plan, replay)
	changed := r
	changed.PhotoRender = new(*r.PhotoRender)
	changed.PhotoRender.Quality = 80
	_, _, err = s.ExportPlanReplay(ctx, "owner", changed)
	require.ErrorIs(t, err, bundle.ErrConflict)
	_, _, err = s.ExportPlanReplay(ctx, "other", r)
	require.ErrorIs(t, err, ErrNotFound)
	var retained, backedUp int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM blobs b WHERE b.hash=? AND (`+blobReferencedSQL("b.hash", blobRootReferences)+`)`, a.SHA256).Scan(&retained))
	require.Equal(t, 1, retained)
	require.NoError(t, s.db.QueryRowContext(ctx, BackupBlobAuthorityCTE()+`SELECT count(*) FROM backup_authorized_blobs WHERE hash=?`, a.SHA256).Scan(&backedUp))
	require.Zero(t, backedUp)
	var planExported bool
	require.NoError(t, exportBundleMetadata(ctx, s.db, func(row any) error {
		if record, ok := row.(metadataExportRecord); ok && record.Kind == "plan" && record.ID == plan.ID {
			planExported = true
		}
		return nil
	}))
	require.False(t, planExported)
	// Tags change pixels' metadata snapshot even when the source bytes remain the same.
	r.OperationID = uuid.New().String()
	tag, err := s.CreateTag(ctx, "shared")
	require.NoError(t, err)
	current, err := s.NodeByID(ctx, n.ID)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, n.ID, current.Revision)
	require.NoError(t, err)
	_, err = s.SealPhotoExportPlan(ctx, "owner", r, []PreparedPhotoExport{a})
	require.ErrorIs(t, err, bundle.ErrConflict)
}

func TestPhotoExportResolvesCompleteScopeAndSelectedDisplayMembers(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	var selected []string
	for index := range MaxDocumentCatalogPageSize + 1 {
		n := browsePhotoNode(t, s, fmt.Sprintf("frame-%04d.jpg", index), fakeHash("a4"), "image/jpeg")
		if index == MaxDocumentCatalogPageSize {
			a, err := s.PhotoAssetForNode(ctx, n.ID)
			require.NoError(t, err)
			selected = []string{a.ID}
		}
	}
	selection := bundle.PhotoExportSelection{Query: snapshotTestQuery(t, `{ "sort": { "field": "name", "direction": "asc" } }`)}
	members, _, err := s.ResolvePhotoExportMembers(ctx, selection)
	require.ErrorIs(t, err, bundle.ErrLimit)
	require.Empty(t, members)
	selection.AssetIDs = selected
	members, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.NoError(t, err)
	require.Len(t, members, 1)
	selection.AssetIDs = []string{uuid.New().String()}
	_, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.ErrorIs(t, err, bundle.ErrConflict)
	selection.Hidden = true
	_, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.ErrorIs(t, err, ErrHiddenLocked)
}

func TestPhotoExportCancellationAndInvalidProfile(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := s.ResolvePhotoExportMembers(ctx, bundle.PhotoExportSelection{Query: snapshotTestQuery(t, `{ "sort": { "field": "name", "direction": "asc" } }`)})
	require.Error(t, err)
	_, err = s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{PhotoRender: &bundle.PhotoRenderProfile{Format: "jpeg", Quality: 0}})
	require.ErrorIs(t, err, bundle.ErrConflict)
}

func TestPhotoExportPreparationAdmissionAndCapacity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	release, err := s.AcquirePhotoExportPreparation(t.Context())
	require.NoError(t, err)
	waiting, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.AcquirePhotoExportPreparation(waiting)
	require.ErrorIs(t, err, context.Canceled)
	release()
	release, err = s.AcquirePhotoExportPreparation(t.Context())
	require.NoError(t, err)
	defer release()
	n := browsePhotoNode(t, s, "capacity.jpg", fakeHash("a5"), "image/jpeg")
	m := bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}
	var source bundle.Source
	for range 32 {
		source, err = s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{m}}, nil)
		require.NoError(t, err)
	}
	r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "png", Quality: 90}}
	_, found, err := s.ExportPlanReplay(t.Context(), "owner", r)
	require.False(t, found)
	require.ErrorIs(t, err, bundle.ErrLimit)
	_, err = s.SealPhotoExportPlan(t.Context(), "owner", r, nil)
	require.ErrorIs(t, err, bundle.ErrLimit)
}
