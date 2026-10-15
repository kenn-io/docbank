package store

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"uuid"
)

func TestPhotoExportPlanRetainsAndRechecksCapturedAuthority(t *testing.T) {
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
	r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "png", Quality: 90, IncludeMetadata: true, RemoveGPS: true}}
	inputs, err := s.ExportPhotoInputs(ctx, "owner", r)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	a := PreparedPhotoExport{Input: inputs[0], Receipt: bundle.PhotoRenderReceipt{Version: bundle.PhotoRenderReceiptVersion, Profile: r.PhotoRender.Canonical(), Source: m, Width: 10, Height: 20, EmbeddedPreview: true}, SHA256: fakeHash("b3"), Size: 99, Physical: BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 99, Created: true}}
	asset, err := s.PhotoAssetForNode(ctx, n.ID)
	require.NoError(t, err)
	rating := 4
	_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: asset.Files[0].Revision, Patch: PhotoAuthoredPatch{Rating: &rating}}})
	require.NoError(t, err)
	pinnedRequest := r
	pinnedRequest.SourceID, pinnedRequest.MemberHash = pinnedSource.ID, pinnedSource.MemberHash
	_, err = s.ExportPhotoInputs(ctx, "owner", pinnedRequest)
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.ErrorContains(t, err, fmt.Sprintf("photo %d", n.ID))
	require.ErrorContains(t, err, n.Name)
	_, err = s.SealPhotoExportPlan(ctx, "owner", r, []PreparedPhotoExport{a})
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.ErrorContains(t, err, fmt.Sprintf("photo %d", n.ID))
	require.ErrorContains(t, err, n.Name)
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
	r.PhotoRender.Quality = 20
	replay, found, err := s.ExportPlanReplay(ctx, "owner", r)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, plan, replay)
	changed := r
	changed.PhotoRender = new(*r.PhotoRender)
	changed.PhotoRender.Format = "jpeg"
	changed.PhotoRender.Quality = 80
	_, _, err = s.ExportPlanReplay(ctx, "owner", changed)
	require.ErrorIs(t, err, bundle.ErrConflict)
	_, _, err = s.ExportPlanReplay(ctx, "other", r)
	require.ErrorIs(t, err, ErrNotFound)
	var metadata, backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &metadata))
	snapshot, err := s.BeginMetadataSnapshot(ctx)
	require.NoError(t, err)
	require.NoError(t, snapshot.ExportBackup(ctx, &backup))
	require.NoError(t, snapshot.Close())
	for _, data := range [][]byte{metadata.Bytes(), backup.Bytes()} {
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(data)))
		_, err = restored.ExportPlan(ctx, "owner", plan.ID)
		require.ErrorIs(t, err, ErrNotFound)
		restoredPlan, err := loadExportPlan(t.Context(), restored.db, plan.ID)
		require.NoError(t, err)
		require.Equal(t, plan, restoredPlan)
		role, err := a.role()
		require.NoError(t, err)
		var docs []bundle.Document
		require.NoError(t, restored.WalkExportDocuments(t.Context(), plan.ID, func(d bundle.Document) error { docs = append(docs, d); return nil }))
		require.Len(t, docs, 1)
		require.Equal(t, m, docs[0].Member)
		require.Equal(t, []bundle.Role{role}, docs[0].Roles)
		fingerprint, err := exportPlanFingerprint(t.Context(), restored.db, restoredPlan)
		require.NoError(t, err)
		require.Equal(t, plan.Fingerprint, fingerprint)
		var restoredRoots int
		require.NoError(t, restored.db.QueryRowContext(t.Context(), `SELECT count(*) FROM export_role_roots WHERE plan_id=? AND blob_hash=?`, plan.ID, a.SHA256).Scan(&restoredRoots))
		require.Equal(t, 1, restoredRoots)
	}
	// Tags change pixels' metadata snapshot even when the source bytes remain the same.
	r.OperationID = uuid.New().String()
	tag, err := s.CreateTag(ctx, "shared")
	require.NoError(t, err)
	current, err := s.NodeByID(ctx, n.ID)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, n.ID, current.Revision)
	require.NoError(t, err)
	inputs, err = s.ExportPhotoInputs(ctx, "owner", r)
	require.NoError(t, err)
	require.Equal(t, []string{"shared"}, inputs[0].Keywords)
	_, err = s.SealPhotoExportPlan(ctx, "owner", r, []PreparedPhotoExport{a})
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err = s.PhotoAssetForNode(ctx, n.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, _, err = s.ResolvePhotoExportMembers(ctx, bundle.PhotoExportSelection{AssetIDs: []string{asset.ID}, Query: snapshotTestQuery(t, `{ "sort": { "field": "name", "direction": "asc" } }`)})
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.NotContains(t, err.Error(), n.Name)
	require.ErrorContains(t, err, "photo "+asset.ID)
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
	require.NotContains(t, err.Error(), n.Name)
	_, err = s.ExportPhotoInputs(unlocked, "owner", r)
	require.ErrorIs(t, err, ErrHiddenLocked)
	require.NotContains(t, err.Error(), n.Name)
	_, err = s.ExportPlan(ctx, "owner", plan.ID)
	require.NoError(t, err)
	token, _, err = s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked = WithPhotoHiddenToken(ctx, token)
	for _, hidden := range []bool{false, true} {
		asset, err = s.PhotoAssetForNode(unlocked, n.ID)
		require.NoError(t, err)
		asset, err = s.SetPhotoAssetHidden(unlocked, asset.ID, asset.Revision, hidden)
		require.NoError(t, err)
		selection := bundle.PhotoExportSelection{Hidden: hidden, Query: snapshotTestQuery(t, `{ "sort": { "field": "name", "direction": "asc" } }`)}
		source, err = s.CreateResolvedExportSource(unlocked, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "photos", Photos: &selection}, func(ctx context.Context) ([]bundle.Member, string, error) {
			return s.ResolvePhotoExportMembers(ctx, selection)
		})
		require.NoError(t, err)
		r.OperationID, r.SourceID, r.MemberHash = uuid.New().String(), source.ID, source.MemberHash
		inputs, err = s.ExportPhotoInputs(unlocked, "owner", r)
		require.NoError(t, err)
		a.Input, a.Receipt.Source = inputs[0], inputs[0].Member
		_, err = s.SetPhotoAssetHidden(unlocked, asset.ID, asset.Revision, !hidden)
		require.NoError(t, err)
		_, err = s.ExportPhotoInputs(unlocked, "owner", r)
		require.ErrorIs(t, err, bundle.ErrConflict)
		require.ErrorContains(t, err, "left the selected scope")
		_, err = s.SealPhotoExportPlan(unlocked, "owner", r, []PreparedPhotoExport{a})
		require.ErrorIs(t, err, bundle.ErrConflict)
		require.ErrorContains(t, err, "left the selected scope")
	}
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
	selection.Query.Filters.AssetIDs = selected
	_, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.NoError(t, err)
	selection.Query.Filters.AssetIDs = []string{uuid.New().String()}
	_, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.ErrorContains(t, err, fmt.Sprintf("photo %d (frame-%04d.jpg)", members[0].NodeID, MaxDocumentCatalogPageSize))
	selection.Query.Filters.AssetIDs = nil
	selection.Query.Filters.CollapseDuplicates = true
	_, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.ErrorContains(t, err, fmt.Sprintf("photo %d (frame-%04d.jpg)", members[0].NodeID, MaxDocumentCatalogPageSize))
	selection.Query.Filters.CollapseDuplicates = false
	selection.Query.Text = "name:frame-0000.jpg"
	_, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.ErrorContains(t, err, fmt.Sprintf("photo %d (frame-%04d.jpg)", members[0].NodeID, MaxDocumentCatalogPageSize))
	_, _, err = s.Trash(ctx, members[0].NodeID, members[0].Revision)
	require.NoError(t, err)
	selection.Query.Text = ""
	_, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.ErrorContains(t, err, fmt.Sprintf("photo %d (frame-%04d.jpg)", members[0].NodeID, MaxDocumentCatalogPageSize))
	selection.AssetIDs = []string{uuid.New().String()}
	_, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.ErrorContains(t, err, "photo "+selection.AssetIDs[0])
	selection.Hidden = true
	_, _, err = s.ResolvePhotoExportMembers(ctx, selection)
	require.ErrorIs(t, err, ErrHiddenLocked)
}

func TestPhotoExportRejectsNonDisplayMember(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	file := fileByRole(asset.Files, PhotoRoleImage)
	n, err := s.NodeByID(ctx, file.NodeID)
	require.NoError(t, err)
	m := bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}
	source, err := s.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{m}}, nil)
	require.NoError(t, err)
	_, err = s.ExportPhotoInputs(ctx, "owner", bundle.PlanRequest{SourceID: source.ID, MemberHash: source.MemberHash})
	require.ErrorIs(t, err, bundle.ErrConflict)
	require.ErrorContains(t, err, "isn't the photo's display file")
}

func TestPhotoExportSourceRejectsDuplicateAssetIDsBeforeReservation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	id := uuid.New().String()
	request := bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "photos", Photos: &bundle.PhotoExportSelection{Query: snapshotTestQuery(t, `{ "sort": { "field": "name", "direction": "asc" } }`), AssetIDs: []string{id, id}}}
	_, err := s.CreateExportSource(t.Context(), "owner", request, func(context.Context) ([]bundle.Member, error) {
		t.Fatal("duplicate selection reached source resolution")
		return nil, nil
	})
	require.ErrorIs(t, err, bundle.ErrConflict)
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM export_sources`).Scan(&count))
	require.Zero(t, count)
}

func TestPhotoExportCancellationAndPlanOptionValidation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := s.ResolvePhotoExportMembers(ctx, bundle.PhotoExportSelection{Query: snapshotTestQuery(t, `{ "sort": { "field": "name", "direction": "asc" } }`)})
	require.ErrorIs(t, err, context.Canceled)
	request := bundle.PlanRequest{Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "png", RemoveGPS: true}}
	for _, policy := range []string{"", "preserve", "collapse_exact_content"} {
		request.DuplicatePolicy = policy
		err = validatePhotoPlanRequest(&request)
		if policy == "collapse_exact_content" {
			require.ErrorIs(t, err, bundle.ErrConflict)
		} else {
			require.NoError(t, err)
			require.False(t, request.PhotoRender.RemoveGPS)
		}
	}
	request.DuplicatePolicy = ""
	request.VolumeLimits = &bundle.VolumeLimits{Roles: 1, RoleBytes: 1024}
	require.ErrorIs(t, validatePhotoPlanRequest(&request), bundle.ErrConflict)
}

func TestPhotoExportPreparationAdmissionAndExportCapacity(t *testing.T) {
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
	release()
	n := browsePhotoNode(t, s, "capacity.jpg", fakeHash("a5"), "image/jpeg")
	m := bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}
	var source bundle.Source
	for range 32 {
		source, err = s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{m}}, nil)
		require.NoError(t, err)
	}
	_, err = s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{m}}, nil)
	require.ErrorIs(t, err, bundle.ErrLimit)
	r := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "photo_rendered"}}, PhotoRender: &bundle.PhotoRenderProfile{Format: "png", Quality: 90}}
	_, found, err := s.ExportPlanReplay(t.Context(), "owner", r)
	require.False(t, found)
	require.ErrorIs(t, err, bundle.ErrLimit)
	require.ErrorContains(t, err, "exports are at capacity; released exports free their slots within about ten minutes")
	_, err = s.SealPhotoExportPlan(t.Context(), "owner", r, nil)
	require.ErrorIs(t, err, bundle.ErrLimit)
}

func TestPhotoExportInputCaptureBoundsKeywordsWithoutWaitingForWriter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newTestStore(t)
	n := browsePhotoNode(t, s, "reader.jpg", fakeHash("a4"), "image/jpeg")
	m := bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size}
	source, err := s.CreateExportSource(ctx, "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{m}}, nil)
	require.NoError(t, err)
	tags, err := s.writeDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tags.Rollback() }()
	for index := range 1001 {
		id := uuid.New().String()
		_, err = tags.ExecContext(ctx, `INSERT INTO tags(id,name) VALUES(?,?)`, id, fmt.Sprintf("tag-%04d", index))
		require.NoError(t, err)
		_, err = tags.ExecContext(ctx, `INSERT INTO node_tags(node_id,tag_id) VALUES(?,?)`, n.ID, id)
		require.NoError(t, err)
	}
	require.NoError(t, tags.Commit())
	writer, err := s.writeDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request := bundle.PlanRequest{SourceID: source.ID, MemberHash: source.MemberHash, PhotoRender: &bundle.PhotoRenderProfile{Format: "png"}}
	inputs, err := s.ExportPhotoInputs(bounded, "owner", request)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	require.Empty(t, inputs[0].Keywords)
	request.PhotoRender.IncludeMetadata = true
	inputs, err = s.ExportPhotoInputs(bounded, "owner", request)
	require.NoError(t, err)
	require.Len(t, inputs[0].Keywords, 1001)
	require.NoError(t, writer.Rollback())
	tag, err := s.CreateTag(ctx, strings.Repeat("x", maxPhotoExportKeywordBytes))
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, n.ID, n.Revision)
	require.NoError(t, err)
	_, err = s.ExportPhotoInputs(ctx, "owner", request)
	require.ErrorIs(t, err, bundle.ErrLimit)
	require.ErrorContains(t, err, fmt.Sprintf("photo %d", n.ID))
	request.PhotoRender.IncludeMetadata = false
	_, err = s.ExportPhotoInputs(ctx, "owner", request)
	require.NoError(t, err)
}

func TestPhotoExportSelectionIgnoresRelevanceOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	var nodes []int64
	var assets []string
	for _, name := range []string{"harbor-a.jpg", "harbor-b.jpg", "harbor-c.jpg"} {
		n := browsePhotoNode(t, s, name, fakeHash("a5"), "image/jpeg")
		a, err := s.PhotoAssetForNode(ctx, n.ID)
		require.NoError(t, err)
		nodes, assets = append(nodes, n.ID), append(assets, a.ID)
	}
	search := snapshotTestQuery(t, `{ "text": "harbor", "syntax": "simple", "sort": { "field": "relevance", "direction": "desc" } }`)
	scope, _, err := s.ResolvePhotoExportMembers(ctx, bundle.PhotoExportSelection{Query: search})
	require.NoError(t, err)
	require.Len(t, scope, 3)
	members, _, err := s.ResolvePhotoExportMembers(ctx, bundle.PhotoExportSelection{Query: search, AssetIDs: assets[2:]})
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, nodes[2], members[0].NodeID)
}
