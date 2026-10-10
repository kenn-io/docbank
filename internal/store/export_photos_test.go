package store

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

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
	pinnedMember := m
	pinnedMember.Revision = n.Revision
	pinnedSource, err := s.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{pinnedMember}}, nil)
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
	pinnedRequest := r
	pinnedRequest.SourceID, pinnedRequest.MemberHash = pinnedSource.ID, pinnedSource.MemberHash
	_, err = s.ExportPhotoInputs(ctx, "owner", pinnedRequest)
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.ErrorContains(t, err, fmt.Sprintf("photo %d", n.ID))
	_, err = s.SealPhotoExportPlan(ctx, "owner", r, []PreparedPhotoExport{a})
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.ErrorContains(t, err, fmt.Sprintf("photo %d", n.ID))
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
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err = s.PhotoAssetForNode(ctx, n.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	members, _, err := s.ResolvePhotoExportMembers(unlocked, bundle.PhotoExportSelection{Hidden: true, AssetIDs: []string{asset.ID}, Query: snapshotTestQuery(t, `{ "sort": { "field": "name", "direction": "asc" } }`)})
	require.NoError(t, err)
	source, err = s.CreateExportSource(unlocked, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: members}, nil)
	require.NoError(t, err)
	r.SourceID, r.MemberHash = source.ID, source.MemberHash
	inputs, err = s.ExportPhotoInputs(unlocked, "owner", r)
	require.NoError(t, err)
	a.Input, a.Receipt.Source = inputs[0], members[0]
	plan, err = s.SealPhotoExportPlan(unlocked, "owner", r, []PreparedPhotoExport{a})
	require.NoError(t, err)
	r.OperationID = uuid.New().String()
	require.NoError(t, s.LockPhotoHidden(ctx))
	_, err = s.SealPhotoExportPlan(unlocked, "owner", r, []PreparedPhotoExport{a})
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.ExportPhotoInputs(unlocked, "owner", r)
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.ExportPlan(ctx, "owner", plan.ID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE export_sources SET expires_at=? WHERE id=?`, time.Now().Add(24*time.Hour).UTC().Format(timestampLayout), source.ID)
	require.NoError(t, err)
	var exported bytes.Buffer
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	require.NoError(t, snapshot.ExportBackup(ctx, &exported))
	require.NoError(t, snapshot.Close())
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	var sources, membersCount int
	require.NoError(t, restored.db.QueryRowContext(ctx, `SELECT count(*) FROM export_sources`).Scan(&sources))
	require.Equal(t, 1, sources)
	require.NoError(t, restored.db.QueryRowContext(ctx, `SELECT count(*) FROM export_members`).Scan(&membersCount))
	require.Equal(t, 1, membersCount)
	var restoredSourceID string
	require.NoError(t, restored.db.QueryRowContext(ctx, `SELECT id FROM export_sources`).Scan(&restoredSourceID))
	require.Equal(t, pinnedSource.ID, restoredSourceID)
	for range 31 {
		_, err = restored.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{m}}, nil)
		require.NoError(t, err)
	}
	_, err = restored.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{m}}, nil)
	require.ErrorIs(t, err, bundle.ErrLimit)
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
	selection.Query.Text = "name:frame-0000.jpg"
	_, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.ErrorIs(t, err, bundle.ErrConflict)
	selection.Query.Text = ""
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
