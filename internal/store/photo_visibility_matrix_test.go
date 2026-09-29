package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
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
	})
	t.Run("deleted", func(t *testing.T) {
		_, err := f.s.PhotoAssetForNode(WithPhotoOwner(f.ctx, f.first.ID), f.firstNode.ID)
		require.NoError(t, err)
		_, err = f.s.db.Exec(`DELETE FROM photo_files WHERE node_id=?`, f.firstNode.ID)
		require.NoError(t, err)
		_, err = f.s.PhotoAssetForNode(WithPhotoOwner(f.ctx, f.first.ID), f.firstNode.ID)
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestPhotoVisibilityContentVersions(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	version, err := f.s.ContentVersionByID(f.ctx, f.firstNode.CurrentVersionID)
	require.NoError(t, err)
	require.NoError(t, f.s.CheckPhotoVisibilityForVersion(WithPhotoOwner(f.ctx, f.first.ID), version.ID))
	require.ErrorIs(t, f.s.CheckPhotoVisibilityForVersion(WithPhotoOwner(f.ctx, f.second.ID), version.ID), ErrNotFound)
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
}

func TestPhotoVisibilityExports(t *testing.T) {
	t.Parallel()
	f := newPhotoVisibilityFixture(t)
	source, err := f.s.CreateExportSource(WithPhotoOwner(f.ctx, f.first.ID), "owner", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit", Members: []bundle.Member{{NodeID: f.firstNode.ID, VersionID: f.firstNode.CurrentVersionID, SHA256: f.firstNode.BlobHash, Size: f.firstNode.Size}}}, nil)
	require.NoError(t, err)
	plan, err := f.s.CreateExportPlan(WithPhotoOwner(f.ctx, f.first.ID), "owner", bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	require.NoError(t, err)
	job, err := f.s.QueueExportJob(WithPhotoOwner(f.ctx, f.first.ID), "owner", bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint})
	require.NoError(t, err)
	assert.Equal(t, f.first.ID, job.PhotoOwnerID)
	assert.True(t, job.PhotoOwnerBound)
	_, err = f.s.db.Exec(`UPDATE photo_assets SET owner_id=? WHERE asset_id=?`, f.second.ID, f.firstAsset.ID)
	require.NoError(t, err)
	require.ErrorIs(t, f.s.CheckExportPlanPhotoVisibility(f.ctx, plan.ID), ErrNotFound)
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
