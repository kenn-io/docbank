package store

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhotoVisibilityDecision(t *testing.T) {
	t.Parallel()
	stamp := "2026-09-28T00:00:00.000000000Z"
	for _, test := range []struct {
		name                          string
		assetOwner, requestOwner      string
		hidden, unlocked, wantVisible bool
	}{
		{name: "unowned"},
		{name: "matching owner", assetOwner: "owner", requestOwner: "owner", wantVisible: true},
		{name: "mismatched owner", assetOwner: "owner", requestOwner: "other"},
		{name: "hidden owner", assetOwner: "owner", requestOwner: "owner", hidden: true},
		{name: "unlocked hidden owner", assetOwner: "owner", requestOwner: "owner", hidden: true, unlocked: true, wantVisible: true},
		{name: "unlock cannot cross owner", assetOwner: "owner", requestOwner: "other", hidden: true, unlocked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var hiddenAt *string
			if test.hidden {
				hiddenAt = &stamp
			}
			assert.Equal(t, test.wantVisible, PhotoVisibilityDecision(test.assetOwner, test.requestOwner, hiddenAt, test.unlocked))
		})
	}
}

func TestPhotoOwnersDefaultLifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	owners, err := s.PhotoOwners(ctx)
	require.NoError(t, err)
	assert.Empty(t, owners)
	type ownerResult struct {
		owner PhotoOwner
		err   error
	}
	ownerResults := make(chan ownerResult, 16)
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			owner, err := s.EnsureDefaultPhotoOwner(ctx)
			ownerResults <- ownerResult{owner: owner, err: err}
		})
	}
	group.Wait()
	close(ownerResults)
	var first PhotoOwner
	count := 0
	for result := range ownerResults {
		require.NoError(t, result.err)
		owner := result.owner
		count++
		if first.ID == "" {
			first = owner
		}
		require.Equal(t, first.ID, owner.ID)
	}
	require.Equal(t, 16, count)
	require.NotEmpty(t, first.ID)
	second, err := s.EnsureDefaultPhotoOwner(ctx)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	require.NoError(t, s.RemovePhotoOwner(ctx, first.ID, first.Revision))
	third, err := s.EnsureDefaultPhotoOwner(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, third.ID)
	assert.Equal(t, "Default", third.Name)
}

func TestPhotoOwnersCRUD(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	owner, err := s.CreatePhotoOwner(ctx, "Alice")
	require.NoError(t, err)
	renamed, err := s.RenamePhotoOwner(ctx, owner.ID, owner.Revision, "Alice Smith")
	require.NoError(t, err)
	assert.Equal(t, int64(2), renamed.Revision)
	noOp, err := s.RenamePhotoOwner(ctx, owner.ID, renamed.Revision, "Alice Smith")
	require.NoError(t, err)
	assert.Equal(t, renamed, noOp)
	_, err = s.RenamePhotoOwner(ctx, owner.ID, renamed.Revision-1, "Stale")
	require.ErrorIs(t, err, ErrStaleRevision)

	image, err := s.CreateFile(WithPhotoOwner(ctx, owner.ID), s.RootID(), "alice.jpg", fakeHash("owner-a"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(WithPhotoOwner(ctx, owner.ID), image.ID)
	require.NoError(t, err)
	assert.Equal(t, owner.ID, *asset.OwnerID)
	err = s.RemovePhotoOwner(ctx, owner.ID, noOp.Revision)
	require.ErrorIs(t, err, ErrPhotoOwnerReferenced)
	renamedAgain, err := s.RenamePhotoOwner(ctx, owner.ID, renamed.Revision, "Alice Again")
	require.NoError(t, err)
	err = s.RemovePhotoOwner(ctx, owner.ID, renamed.Revision)
	require.ErrorIs(t, err, ErrStaleRevision)
	err = s.RemovePhotoOwner(ctx, owner.ID, renamedAgain.Revision)
	require.ErrorIs(t, err, ErrPhotoOwnerReferenced)

	_, err = s.PhotoAssetForNode(WithPhotoOwner(ctx, "00000000-0000-4000-8000-000000000000"), image.ID)
	require.ErrorIs(t, err, ErrNotFound)
	empty, err := s.CreatePhotoOwner(ctx, "Empty")
	require.NoError(t, err)
	assert.NoError(t, s.RemovePhotoOwner(ctx, empty.ID, empty.Revision))
}

func TestPhotoOwnersMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	owner, err := s.EnsureDefaultPhotoOwner(ctx)
	require.NoError(t, err)
	image, err := s.CreateFile(WithPhotoOwner(ctx, owner.ID), s.RootID(), "roundtrip.jpg", fakeHash("f1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(WithPhotoOwner(ctx, owner.ID), image.ID)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	assert.Contains(t, exported.String(), `"type":"photo_owner"`)
	assert.Contains(t, exported.String(), `"owner_id":"`+owner.ID+`"`)
	assert.Contains(t, exported.String(), `"owner_id":"`+owner.ID+`"`)

	clone := newTestStore(t)
	require.NoError(t, clone.ImportMetadata(context.Background(), bytes.NewReader(exported.Bytes())))
	got, err := clone.PhotoOwner(ctx, owner.ID)
	require.NoError(t, err)
	assert.Equal(t, owner, got)
	gotAsset, err := clone.PhotoAssetForNode(WithPhotoOwner(ctx, owner.ID), image.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.ID, gotAsset.ID)

	bad := bytes.Replace(exported.Bytes(), []byte(owner.ID), []byte("00000000-0000-4000-8000-000000000000"), 1)
	invalid := newTestStore(t)
	err = invalid.ImportMetadata(ctx, bytes.NewReader(bad))
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotFound)
	missingOwnerLines := bytes.Split(exported.Bytes(), []byte("\n"))
	for index, line := range missingOwnerLines {
		if bytes.Contains(line, []byte(`"type":"photo_asset"`)) {
			missingOwnerLines[index] = bytes.Replace(line, []byte(`"owner_id":"`+owner.ID+`"`), []byte(`"owner_id":null`), 1)
			break
		}
	}
	missingOwner := newTestStore(t)
	require.Error(t, missingOwner.ImportMetadata(ctx, bytes.NewReader(bytes.Join(missingOwnerLines, []byte("\n")))))
}

func TestPhotoAssetOwnerIsRequiredBySchema(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.db.ExecContext(t.Context(), `INSERT INTO photo_assets(asset_id,kind,revision,owner_id,created_at,updated_at) VALUES(?,?,1,NULL,?,?)`,
		"00000000-0000-4000-8000-000000000099", PhotoKindPhoto, nowRFC3339(), nowRFC3339())
	require.Error(t, err)
}
