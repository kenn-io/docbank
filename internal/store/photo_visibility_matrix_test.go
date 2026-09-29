package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/query"
	"uuid"
)

type photoVisibilityFixture struct {
	s                       *Store
	ctx                     context.Context
	first, second           PhotoOwner
	firstNode, secondNode   Node
	ordinary                Node
	firstAsset, secondAsset PhotoAsset
}

func newPhotoVisibilityFixture(t *testing.T) photoVisibilityFixture {
	t.Helper()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreatePhotoOwner(ctx, "First")
	require.NoError(t, err)
	second, err := s.CreatePhotoOwner(ctx, "Second")
	require.NoError(t, err)
	firstNode, err := s.CreateFile(WithPhotoOwner(ctx, first.ID), s.RootID(), "first.jpg", fakeHash("face01"), 11, "image/jpeg")
	require.NoError(t, err)
	secondNode, err := s.CreateFile(WithPhotoOwner(ctx, second.ID), s.RootID(), "second.jpg", fakeHash("face02"), 12, "image/jpeg")
	require.NoError(t, err)
	ordinary, err := s.CreateFile(ctx, s.RootID(), "ordinary.txt", fakeHash("face03"), 13, "text/plain")
	require.NoError(t, err)
	firstAsset, err := s.PhotoAssetForNode(WithPhotoOwner(ctx, first.ID), firstNode.ID)
	require.NoError(t, err)
	secondAsset, err := s.PhotoAssetForNode(WithPhotoOwner(ctx, second.ID), secondNode.ID)
	require.NoError(t, err)
	return photoVisibilityFixture{s: s, ctx: ctx, first: first, second: second, firstNode: firstNode, secondNode: secondNode, ordinary: ordinary, firstAsset: firstAsset, secondAsset: secondAsset}
}

func TestPhotoVisibilityPredicateParity(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	require.NoError(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.first.ID), f.firstNode.ID))
	require.ErrorIs(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.first.ID), f.secondNode.ID), ErrNotFound)
	require.NoError(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.second.ID), f.ordinary.ID))
	assert.True(t, PhotoVisibilityDecision(f.first.ID, f.first.ID, nil, false))
	assert.False(t, PhotoVisibilityDecision(f.second.ID, f.first.ID, nil, false))
	for _, test := range []struct {
		name string
		ctx  context.Context
		id   string
		want bool
	}{
		{"own", WithPhotoOwner(f.ctx, f.first.ID), f.firstAsset.ID, true},
		{"foreign", WithPhotoOwner(f.ctx, f.first.ID), f.secondAsset.ID, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			predicate, args, err := photoAssetVisibilitySQL(test.ctx, f.s.db, "a")
			require.NoError(t, err)
			var visible bool
			require.NoError(t, f.s.db.QueryRowContext(t.Context(),
				`SELECT `+predicate+` FROM photo_assets a WHERE a.asset_id=?`, append(args, test.id)...).Scan(&visible))
			assert.Equal(t, test.want, visible)
		})
	}
	_, _, err := photoAssetVisibilitySQL(WithPhotoOwner(f.ctx, "not-an-owner"), f.s.db, "a")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoVisibilityMembership(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	_, err := f.s.PhotoAssetForNode(WithPhotoOwner(f.ctx, f.first.ID), f.secondNode.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.s.PhotoAssetForNode(WithPhotoOwner(f.ctx, f.first.ID), f.ordinary.ID)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.first.ID), f.ordinary.ID))
}

func TestPhotoVisibilityExcluded(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	asset, err := f.s.SetPhotoAssetExcluded(WithPhotoOwner(f.ctx, f.first.ID), f.firstAsset.ID, f.firstAsset.Revision, true)
	require.NoError(t, err)
	assert.NotNil(t, asset.ExcludedAt)
	_, err = f.s.PhotoAssetByID(WithPhotoOwner(f.ctx, f.second.ID), f.firstAsset.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.s.PhotoAssetByID(WithPhotoOwner(f.ctx, f.first.ID), f.firstAsset.ID)
	require.NoError(t, err)
}

func TestPhotoVisibilityEnrollment(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	asset, err := f.s.PhotoAssetForNode(WithPhotoOwner(f.ctx, f.first.ID), f.firstNode.ID)
	require.NoError(t, err)
	assert.Equal(t, f.first.ID, *asset.OwnerID)
	_, err = f.s.PhotoAssetForNode(WithPhotoOwner(f.ctx, f.second.ID), f.firstNode.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoVisibilityDisplay(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	asset, err := f.s.PhotoAssetForNode(WithPhotoOwner(f.ctx, f.first.ID), f.firstNode.ID)
	require.NoError(t, err)
	_, err = f.s.SetPhotoDisplay(WithPhotoOwner(f.ctx, f.first.ID), asset.ID, asset.Revision, nil)
	require.NoError(t, err)
	_, err = f.s.PhotoAssetByID(WithPhotoOwner(f.ctx, f.second.ID), asset.ID)
	require.ErrorIs(t, err, ErrNotFound)
	raw, err := f.s.CreateFile(WithPhotoOwner(f.ctx, f.first.ID), f.s.RootID(), "capture.cr2", fakeHash("display-raw"), 14, "application/octet-stream")
	require.NoError(t, err)
	asset, err = f.s.PromotePhotoNode(WithPhotoOwner(f.ctx, f.first.ID), raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	sidecar, err := f.s.CreateFile(WithPhotoOwner(f.ctx, f.first.ID), f.s.RootID(), "capture.xmp", fakeHash("display-sidecar"), 15, "application/octet-stream")
	require.NoError(t, err)
	rawFile := fileByRole(asset.Files, PhotoRoleRAW)
	asset, err = f.s.AttachPhotoFile(WithPhotoOwner(f.ctx, f.first.ID), asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &rawFile.ID)
	require.NoError(t, err)
	asset, err = f.s.SetPhotoDisplay(WithPhotoOwner(f.ctx, f.first.ID), asset.ID, asset.Revision, &rawFile.ID)
	require.NoError(t, err)
	assert.Len(t, asset.Files, 2)
	assert.Equal(t, rawFile.ID, *asset.DisplayOverrideFileID)
	assert.Equal(t, rawFile.ID, *asset.DisplayFileID)
}

func TestPhotoVisibilityNodeStates(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	t.Run("live", func(t *testing.T) {
		require.NoError(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.first.ID), f.firstNode.ID))
		require.ErrorIs(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.first.ID), f.secondNode.ID), ErrNotFound)
	})
	t.Run("trash", func(t *testing.T) {
		_, _, err := f.s.Trash(WithPhotoOwner(f.ctx, f.first.ID), f.firstNode.ID, f.firstNode.Revision)
		require.NoError(t, err)
		require.ErrorIs(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.second.ID), f.firstNode.ID), ErrNotFound)
		directory, err := f.s.Mkdir(f.ctx, f.s.RootID(), "nested")
		require.NoError(t, err)
		child, err := f.s.CreateFile(WithPhotoOwner(f.ctx, f.first.ID), directory.ID, "nested.jpg", fakeHash("nested-photo"), 13, "image/jpeg")
		require.NoError(t, err)
		_, _, err = f.s.Trash(WithPhotoOwner(f.ctx, f.second.ID), directory.ID, directory.Revision)
		require.ErrorIs(t, err, ErrNotFound)
		stillDirectory, err := f.s.NodeByID(f.ctx, directory.ID)
		require.NoError(t, err)
		assert.Nil(t, stillDirectory.TrashedAt)
		require.NoError(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.first.ID), child.ID))
	})
	t.Run("deleted", func(t *testing.T) {
		deleted, err := f.s.CreateFile(WithPhotoOwner(f.ctx, f.first.ID), f.s.RootID(), "deleted.jpg", fakeHash("deleted-photo"), 16, "image/jpeg")
		require.NoError(t, err)
		deletedAsset, err := f.s.PhotoAssetForNode(WithPhotoOwner(f.ctx, f.first.ID), deleted.ID)
		require.NoError(t, err)
		_, _, err = f.s.Trash(WithPhotoOwner(f.ctx, f.first.ID), deleted.ID, deleted.Revision)
		require.NoError(t, err)
		_, err = f.s.TrashEmpty(f.ctx, 0, true)
		require.NoError(t, err)
		_, err = f.s.NodeByID(WithPhotoOwner(f.ctx, f.first.ID), deleted.ID)
		require.ErrorIs(t, err, ErrNotFound)
		_, err = f.s.ContentVersionByID(WithPhotoOwner(f.ctx, f.first.ID), deleted.CurrentVersionID)
		require.ErrorIs(t, err, ErrNotFound)
		asset, err := f.s.PhotoAssetByID(WithPhotoOwner(f.ctx, f.first.ID), deletedAsset.ID)
		require.NoError(t, err)
		assert.Empty(t, asset.Files)
	})
}

func TestPhotoVisibilityContentVersions(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	version, err := f.s.ContentVersionByID(f.ctx, f.firstNode.CurrentVersionID)
	require.NoError(t, err)
	require.NoError(t, f.s.CheckPhotoVisibilityForVersion(WithPhotoOwner(f.ctx, f.first.ID), version.ID))
	require.ErrorIs(t, f.s.CheckPhotoVisibilityForVersion(WithPhotoOwner(f.ctx, f.second.ID), version.ID), ErrNotFound)
	sharedHash := fakeHash("abc123")
	firstShared, err := f.s.CreateFile(WithPhotoOwner(f.ctx, f.first.ID), f.s.RootID(), "shared-first.jpg", sharedHash, 17, "image/jpeg")
	require.NoError(t, err)
	secondShared, err := f.s.CreateFile(WithPhotoOwner(f.ctx, f.second.ID), f.s.RootID(), "shared-second.jpg", sharedHash, 17, "image/jpeg")
	require.NoError(t, err)
	references, total, err := f.s.ContentReferencesByHash(WithPhotoOwner(f.ctx, f.first.ID), sharedHash, 20, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, references, 1)
	assert.Equal(t, firstShared.ID, references[0].Node.ID)
	references, total, err = f.s.ContentReferencesByHash(WithPhotoOwner(f.ctx, f.second.ID), sharedHash, 20, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, references, 1)
	assert.Equal(t, secondShared.ID, references[0].Node.ID)
	historicalID := firstShared.CurrentVersionID
	updated, _, err := f.s.ReplaceContent(WithPhotoOwner(f.ctx, f.first.ID), firstShared.ID, firstShared.Revision, fakeHash("historical-photo"), 18, "image/jpeg")
	require.NoError(t, err)
	require.NoError(t, f.s.CheckPhotoVisibilityForVersion(WithPhotoOwner(f.ctx, f.first.ID), historicalID))
	require.ErrorIs(t, f.s.CheckPhotoVisibilityForVersion(WithPhotoOwner(f.ctx, f.second.ID), historicalID), ErrNotFound)
	versions, total, err := f.s.ContentVersions(WithPhotoOwner(f.ctx, f.first.ID), updated.ID, 20, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.Len(t, versions, 2)
	_, err = f.s.PruneContentVersions(WithPhotoOwner(f.ctx, f.first.ID), updated.ID, updated.Revision,
		VersionPruneSelector{VersionIDs: []string{historicalID}}, true)
	require.NoError(t, err)
	require.ErrorIs(t, f.s.CheckPhotoVisibilityForVersion(WithPhotoOwner(f.ctx, f.first.ID), historicalID), ErrNotFound)
}

func TestPhotoVisibilityPopulations(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	page, err := f.s.ListDocuments(WithPhotoOwner(f.ctx, f.first.ID), DocumentCatalogQuery{PageSize: 20}, nil, DocumentCatalogTraversalNext)
	require.NoError(t, err)
	var names []string
	for _, item := range page.Items {
		names = append(names, item.Name)
		assert.NotEqual(t, "second.jpg", item.Name)
	}
	assert.Contains(t, names, "first.jpg")
}

func TestPhotoVisibilityAggregateRoutes(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	hits, _, err := f.s.SearchPage(WithPhotoOwner(f.ctx, f.first.ID), "first", 20)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
	assert.Equal(t, "first.jpg", hits[0].Node.Name)
	explained, _, err := f.s.SearchExplainedLexicalCandidates(WithPhotoOwner(f.ctx, f.first.ID), "second", 20, SearchOptions{})
	require.NoError(t, err)
	assert.Empty(t, explained)
	filtered, err := f.s.ResolveProcessingSourceFence(WithPhotoOwner(f.ctx, f.first.ID), ProcessingSourceFenceRequest{Filters: &SearchOptions{MIMEType: "image/jpeg"}})
	require.NoError(t, err)
	assert.Equal(t, []string{f.firstNode.CurrentVersionID}, filtered.ContentVersionIDs)
	_, err = f.s.ResolveProcessingSourceFence(WithPhotoOwner(f.ctx, f.first.ID), ProcessingSourceFenceRequest{ContentVersionIDs: []string{f.secondNode.CurrentVersionID}})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoVisibilityCachedResources(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	owner, bound, noOwner, err := f.s.PhotoOwnerForRequest(WithPhotoOwner(f.ctx, f.first.ID))
	require.NoError(t, err)
	assert.Equal(t, f.first.ID, owner)
	assert.True(t, bound)
	assert.False(t, noOwner)
	require.ErrorIs(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.second.ID), f.firstNode.ID), ErrNotFound)
	snapshots := NewQuerySnapshotService(f.s)
	t.Cleanup(func() { require.NoError(t, snapshots.Close()) })
	value, err := query.Parse([]byte(`{"v":1,"filters":{}}`))
	require.NoError(t, err)
	for index := range 51 {
		_, err = f.s.CreateFile(f.ctx, f.s.RootID(), fmt.Sprintf("cached-%02d.txt", index), fakeHash(fmt.Sprintf("cached-%02d", index)), 20, "text/plain")
		require.NoError(t, err)
	}
	_, _, err = f.s.Move(f.ctx, f.ordinary.ID, f.s.RootID(), "ordinary-renamed.txt", f.ordinary.Revision)
	require.NoError(t, err)
	var contentRevision, nodeRevision int64
	require.NoError(t, f.s.db.QueryRowContext(t.Context(), `SELECT node_revision FROM content_versions WHERE version_id=?`, f.ordinary.CurrentVersionID).Scan(&contentRevision))
	require.NoError(t, f.s.db.QueryRowContext(t.Context(), `SELECT revision FROM nodes WHERE id=?`, f.ordinary.ID).Scan(&nodeRevision))
	assert.Greater(t, nodeRevision, contentRevision)
	page, err := snapshots.Create(WithPhotoOwner(f.ctx, f.first.ID), "resource", SnapshotRequest{Query: value, PageSize: 50})
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)
	var ordinaryRow SnapshotRow
	for _, row := range page.Rows {
		if row.NodeID == f.ordinary.ID {
			ordinaryRow = row
			break
		}
	}
	nextPage, err := snapshots.Page(WithPhotoOwner(f.ctx, f.first.ID), "resource", page.SnapshotID, page.NextCursor)
	require.NoError(t, err)
	for _, row := range nextPage.Rows {
		if row.NodeID == f.ordinary.ID {
			ordinaryRow = row
			break
		}
	}
	require.Equal(t, nodeRevision, ordinaryRow.Revision)
	_, err = snapshots.CopyMembers(WithPhotoOwner(f.ctx, f.first.ID), "resource", page.SnapshotID, page.MemberHash)
	require.NoError(t, err)
	_, err = f.s.db.Exec(`UPDATE photo_assets SET owner_id=? WHERE asset_id=?`, f.second.ID, f.firstAsset.ID)
	require.NoError(t, err)
	_, err = snapshots.CopyMembers(WithPhotoOwner(f.ctx, f.first.ID), "resource", page.SnapshotID, page.MemberHash)
	require.ErrorIs(t, err, ErrSnapshotGone)
}

func TestPhotoVisibilityDerivedRoutes(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	require.NoError(t, f.s.CheckPhotoVisibilityForVersion(WithPhotoOwner(f.ctx, f.first.ID), f.firstNode.CurrentVersionID))
	require.ErrorIs(t, f.s.CheckPhotoVisibilityForVersion(WithPhotoOwner(f.ctx, f.second.ID), f.firstNode.CurrentVersionID), ErrNotFound)
}

func TestPhotoVisibilityHistoryRoutes(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	_, err := f.s.NodeByID(WithPhotoOwner(f.ctx, f.first.ID), f.firstNode.ID)
	require.NoError(t, err)
	require.ErrorIs(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.second.ID), f.firstNode.ID), ErrNotFound)
}

func TestPhotoVisibilityMutationAtomicity(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	_, _, err := f.s.Move(WithPhotoOwner(f.ctx, f.first.ID), f.secondNode.ID, f.s.RootID(), "moved.jpg", f.secondNode.Revision)
	require.ErrorIs(t, err, ErrNotFound)
	after, err := f.s.NodeByID(f.ctx, f.secondNode.ID)
	require.NoError(t, err)
	assert.Equal(t, "second.jpg", after.Name)
	moved, _, err := f.s.Move(WithPhotoOwner(f.ctx, f.first.ID), f.ordinary.ID, f.s.RootID(), "ordinary-moved.txt", f.ordinary.Revision)
	require.NoError(t, err)
	assert.Equal(t, "ordinary-moved.txt", moved.Name)
}

func TestPhotoVisibilityExports(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	sourceRequest := bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: f.firstNode.ID, VersionID: f.firstNode.CurrentVersionID, SHA256: f.firstNode.BlobHash, Size: f.firstNode.Size}}}
	source, err := f.s.CreateExportSource(WithPhotoOwner(f.ctx, f.first.ID), "owner", sourceRequest, nil)
	require.NoError(t, err)
	_, err = f.s.CreateExportSource(WithPhotoOwner(f.ctx, f.second.ID), "owner", sourceRequest, nil)
	require.ErrorIs(t, err, ErrNotFound)
	planRequest := bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}}
	plan, err := f.s.CreateExportPlan(WithPhotoOwner(f.ctx, f.first.ID), "owner", planRequest)
	require.NoError(t, err)
	_, err = f.s.CreateExportPlan(WithPhotoOwner(f.ctx, f.second.ID), "owner", planRequest)
	require.ErrorIs(t, err, ErrNotFound)
	job, err := f.s.QueueExportJob(WithPhotoOwner(f.ctx, f.first.ID), "owner", bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.NoError(t, err)
	assert.Equal(t, f.first.ID, job.PhotoOwnerID)
	assert.True(t, job.PhotoOwnerBound)
	_, err = f.s.QueueExportJob(WithPhotoOwner(f.ctx, f.second.ID), "owner", bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, f.s.CheckExportPlanPhotoVisibility(WithPhotoOwner(f.ctx, f.first.ID), plan.ID))
	require.ErrorIs(t, f.s.CheckExportPlanPhotoVisibility(WithPhotoOwner(f.ctx, f.second.ID), plan.ID), ErrNotFound)
	require.ErrorIs(t, f.s.CheckExportPlanPhotoVisibility(WithPhotoOwner(f.ctx, ""), plan.ID), ErrNotFound)
	_, err = f.s.ExportPlan(WithPhotoOwner(f.ctx, f.second.ID), "owner", plan.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.s.db.Exec(`UPDATE photo_assets SET owner_id=? WHERE asset_id=?`, f.second.ID, f.firstAsset.ID)
	require.NoError(t, err)
	require.ErrorIs(t, f.s.CheckExportPlanPhotoVisibility(f.ctx, plan.ID), ErrNotFound)
}

func TestPhotoVisibilityBatesArtifactIDs(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	source := document.PageSource{VersionID: f.firstNode.CurrentVersionID, SHA256: f.firstNode.BlobHash, Size: f.firstNode.Size}
	frame, err := document.NewPDFPageFrame(source, 1, [4]float64{0, 0, 72, 72}, [4]float64{0, 0, 72, 72}, 0)
	require.NoError(t, err)
	require.NoError(t, f.s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return putPageDocument(t.Context(), tx, document.PageDocumentV1{Contract: document.PageFrameContractV1, Source: source, PageCount: 1, Frames: []document.PageFrameV1{frame}})
	}))
	occurrence := strings.Repeat("c", 32)
	snapshot, err := f.s.SealCollectionSnapshot(WithPhotoOwner(f.ctx, f.first.ID), SnapshotSealRequest{
		SnapshotID: uuid.New().String(), Members: []CollectionSnapshotMember{{
			Ordinal: 1, OccurrenceID: occurrence, NodeID: f.firstNode.ID, ContentVersionID: f.firstNode.CurrentVersionID,
			BlobSHA256: f.firstNode.BlobHash, Size: f.firstNode.Size, FamilyID: occurrence, FamilyOrder: 1,
			DisplayName: "first.jpg", FrozenFieldsJSON: "{}", DocumentKind: "other", SourcePageCount: 1,
			SelectedPDFSHA256: f.firstNode.BlobHash,
		}},
	})
	require.NoError(t, err)
	namespace, err := f.s.EnsureBatesNamespace(t.Context(), "OWN", "", 6)
	require.NoError(t, err)
	recipe, err := canonical.Marshal(map[string]any{"contract": "bates-stamp/v1"})
	require.NoError(t, err)
	allocation, err := f.s.ReserveBatesRange(WithPhotoOwner(f.ctx, f.first.ID), BatesPlanRequest{
		OperationID: uuid.New().String(), NamespaceID: namespace.NamespaceID, SnapshotID: snapshot.SnapshotID,
		RecipeSHA256: digestCatalogJSON(recipe), StartAt: 1,
		Pages: []BatesPageInput{{OccurrenceID: occurrence, UnstampedSHA256: f.firstNode.BlobHash, SourcePage: 1, VerifiedPageCount: 1}},
	})
	require.NoError(t, err)
	artifactID := uuid.New().String()
	artifact, err := f.s.PublishBatesArtifact(WithPhotoOwner(f.ctx, f.first.ID), BatesArtifactPublication{
		ArtifactID: artifactID, AllocationID: allocation.AllocationID, BlobSHA256: fakeHash("bada"), Size: 10,
		PageCount: 1, RecipeJSON: recipe, Pages: []BatesArtifactPage{{Ordinal: 1, OccurrenceID: occurrence, SourceBlobSHA256: f.firstNode.BlobHash, SourcePage: 1, OutputPage: 1, Label: allocation.Labels[0].Label}},
	}, BlobPhysical{Encoding: "raw", StoredBytes: 10, Created: true})
	require.NoError(t, err)
	require.Equal(t, artifactID, artifact.ArtifactID)
	read, err := f.s.BatesArtifact(WithPhotoOwner(f.ctx, f.first.ID), allocation.AllocationID)
	require.NoError(t, err)
	require.Equal(t, artifactID, read.ArtifactID)
	listed, err := f.s.BatesArtifacts(WithPhotoOwner(f.ctx, f.first.ID), "", 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, artifactID, listed[0].ArtifactID)
	count, err := f.s.BatesArtifactCount(WithPhotoOwner(f.ctx, f.first.ID))
	require.NoError(t, err)
	require.Equal(t, 1, count)
	_, err = f.s.BatesArtifact(WithPhotoOwner(f.ctx, f.second.ID), allocation.AllocationID)
	require.ErrorIs(t, err, ErrNotFound)
	listed, err = f.s.BatesArtifacts(WithPhotoOwner(f.ctx, f.second.ID), "", 10)
	require.NoError(t, err)
	require.Empty(t, listed)
	count, err = f.s.BatesArtifactCount(WithPhotoOwner(f.ctx, f.second.ID))
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestPhotoOwnerAsyncPropagation(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	ctx := WithPhotoOwnerBinding(context.Background(), f.first.ID, true, false)
	owner, bound, noOwner, err := f.s.PhotoOwnerForRequest(ctx)
	require.NoError(t, err)
	assert.Equal(t, f.first.ID, owner)
	assert.True(t, bound)
	assert.False(t, noOwner)
	source, err := f.s.CreateExportSource(ctx, "detached-owner", bundle.SourceRequest{
		OperationID: uuid.New().String(), Kind: "explicit",
		Members: []bundle.Member{{NodeID: f.firstNode.ID, VersionID: f.firstNode.CurrentVersionID, SHA256: f.firstNode.BlobHash, Size: f.firstNode.Size}},
	}, nil)
	require.NoError(t, err)
	plan, err := f.s.CreateExportPlan(ctx, "detached-owner", bundle.PlanRequest{
		OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash,
		Roles: []bundle.RolePolicy{{Role: "original"}},
	})
	require.NoError(t, err)
	job, err := f.s.QueueExportJob(ctx, "detached-owner", bundle.JobRequest{
		OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint,
	})
	require.NoError(t, err)
	claim, err := f.s.ClaimExportJob(context.Background())
	require.NoError(t, err)
	require.Equal(t, job.ID, claim.Job.ID)
	detached := WithPhotoOwnerBinding(context.Background(), claim.Job.PhotoOwnerID, claim.Job.PhotoOwnerBound, claim.Job.PhotoNoOwner)
	_, err = f.s.ExportPlanForClaim(detached, claim)
	require.NoError(t, err)
	wrongClaim := claim
	wrongClaim.Job.PhotoOwnerID = f.second.ID
	_, err = f.s.ExportPlanForClaim(WithPhotoOwnerBinding(context.Background(), f.second.ID, true, false), wrongClaim)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPhotoOwnersAuditGate(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	plan, err := f.s.PreviewInitialAudit(f.ctx, f.s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = f.s.EnableInitialAudit(f.ctx, plan)
	require.NoError(t, err)
	_, err = f.s.CreatePhotoOwner(f.ctx, "blocked")
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	require.NoError(t, f.s.CheckPhotoVisibilityForNode(WithPhotoOwner(f.ctx, f.first.ID), f.firstNode.ID))
}
