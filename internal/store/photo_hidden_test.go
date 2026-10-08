package store

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func TestPhotoHiddenLifecycleAndBackup(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	s.photoHiddenNow = func() time.Time { return now }
	node, err := s.CreateFile(ctx, s.RootID(), "private.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.ErrorIs(t, err, ErrHiddenNotConfigured)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.NotNil(t, asset.HiddenAt)
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision-1, true)
	require.ErrorIs(t, err, ErrHiddenLocked)
	value, err := query.Parse([]byte(`{"v":1,"syntax":"advanced","mode":"lexical","text":"","sort":{"field":"name","direction":"asc"}}`))
	require.NoError(t, err)
	page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: value}, nil)
	require.NoError(t, err)
	assert.Empty(t, page.Items)
	assert.Zero(t, page.Total)
	_, err = s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: value, Hidden: true}, nil)
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.PhotoAssetByID(ctx, asset.ID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.PhotoAssetForNode(ctx, node.ID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	_, err = s.PhotoAssetByID(unlocked, asset.ID)
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(unlocked, node.ID)
	require.NoError(t, err)
	page, err = s.ListPhotoAssets(unlocked, PhotoBrowseRequest{Query: value, Hidden: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), page.Total)
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	assert.Contains(t, backup.String(), `"hidden_at"`)
	assert.NotContains(t, backup.String(), token)
	assert.NotContains(t, backup.String(), hiddenTokenDigest(token))
	assert.NotContains(t, backup.String(), "photo_hidden_session")
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	_, err = target.ListPhotoAssets(unlocked, PhotoBrowseRequest{Query: value, Hidden: true}, nil)
	require.ErrorIs(t, err, ErrHiddenLocked)
	targetToken, _, err := target.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	page, err = target.ListPhotoAssets(WithPhotoHiddenToken(ctx, targetToken), PhotoBrowseRequest{Query: value, Hidden: true}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	now = now.Add(5 * time.Minute)
	_, err = s.ListPhotoAssets(unlocked, PhotoBrowseRequest{Query: value, Hidden: true}, nil)
	require.ErrorIs(t, err, ErrHiddenLocked)
	require.NoError(t, s.ResetPhotoHidden(ctx))
	asset, err = photoAssetByIDQuery(ctx, s.db, asset.ID)
	require.NoError(t, err)
	assert.NotNil(t, asset.HiddenAt)
	require.NoError(t, s.SetupPhotoHidden(ctx, "replacement"))
	require.NoError(t, s.DisablePhotoHidden(ctx, "replacement"))
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.Nil(t, asset.HiddenAt)
	require.NoError(t, validatePhotoMetadataState(ctx, s.db, currentStorageSchemaVersion))
}

func TestPhotoHiddenFailuresSurviveBackupAndRestart(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	s.photoHiddenNow = func() time.Time { return now }
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	for range 4 {
		_, _, err := s.UnlockPhotoHidden(ctx, "wrong")
		require.ErrorIs(t, err, ErrHiddenPasscode)
	}
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	target := newTestStore(t)
	target.photoHiddenNow = func() time.Time { return now }
	require.NoError(t, target.ImportMetadata(ctx, &backup))
	_, _, err := target.UnlockPhotoHidden(ctx, "wrong")
	var lockout *HiddenLockoutError
	require.ErrorAs(t, err, &lockout)
	require.NoError(t, target.Checkpoint(ctx))
	path, driver := target.path, target.driver
	require.NoError(t, target.Close())
	reopened, err := Open(path, driver)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	reopened.photoHiddenNow = func() time.Time { return now }
	_, _, err = reopened.UnlockPhotoHidden(ctx, "correct")
	require.ErrorAs(t, err, &lockout)
	now = now.Add(5 * time.Minute)
	_, _, err = reopened.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
}

func TestPhotoHiddenChangeRevokesAndFailedDisableIsAtomic(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	require.NoError(t, s.ChangePhotoHidden(ctx, "correct", "replacement"))
	state, err := s.PhotoHiddenState(WithPhotoHiddenToken(ctx, token))
	require.NoError(t, err)
	assert.Nil(t, state.ExpiresAt)
	node, err := s.CreateFile(ctx, s.RootID(), "private.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, err = s.writeDB.Exec(`CREATE TRIGGER deny_unhide BEFORE UPDATE OF hidden_at ON photo_assets BEGIN SELECT RAISE(ABORT,'synthetic refusal'); END`)
	require.NoError(t, err)
	require.Error(t, s.DisablePhotoHidden(ctx, "replacement"))
	state, err = s.PhotoHiddenState(ctx)
	require.NoError(t, err)
	assert.True(t, state.Configured)
	asset, err = photoAssetByIDQuery(ctx, s.db, asset.ID)
	require.NoError(t, err)
	assert.NotNil(t, asset.HiddenAt)
}

func TestPhotoHiddenValidatesBoundedHashesAndPasscodes(t *testing.T) {
	for _, passcode := range []string{"", strings.Repeat("x", 1025)} {
		require.ErrorIs(t, validHiddenPasscode(passcode), ErrInvalidHiddenPasscode)
	}
	for _, hash := range []string{"argon2id$m=19456,t=2,p=1$AA$AA", "argon2id$m=999999,t=2,p=1$AA$AA"} {
		_, _, err := hiddenHashParts(hash)
		require.Error(t, err)
	}
	assert.Equal(t, "", hiddenTokenDigest("AAAA"))
	assert.Equal(t, "", hiddenTokenDigest(strings.Repeat("x", 1000)))
	_, _, err := newTestStore(t).UnlockPhotoHidden(context.Background(), "")
	require.ErrorIs(t, err, ErrInvalidHiddenPasscode)
}

func TestPhotoHiddenAlbumPopulationAndCursor(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	a := albumAsset(t, s, "first.jpg")
	b := albumAsset(t, s, "second.jpg")
	set, err := s.CreatePhotoSet(ctx, "Synthetic album")
	require.NoError(t, err)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{a.ID, b.ID}})
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	_, err = s.SetPhotoAssetHidden(ctx, a.ID, a.Revision, true)
	require.NoError(t, err)
	summary, err := s.PhotoSet(ctx, set.ID, "")
	require.NoError(t, err)
	assert.Equal(t, int64(2), summary.MemberCount)
	assert.Equal(t, int64(1), summary.IncludedCount)
	assert.Equal(t, int64(1), summary.HiddenCount)
	visible := browsePhotoPage(t, s, `{"filters":{"set_ids":["`+set.ID+`"]},"sort":{"field":"name","direction":"asc"}}`)
	require.Len(t, visible.Items, 1)
	assert.Equal(t, b.ID, visible.Items[0].AssetID)
	copy, err := s.CreatePhotoSet(ctx, "Selected query")
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{}`)
	copy, err = s.ChangePhotoSetMembers(ctx, copy.ID, copy.Revision, true, PhotoSetSelection{Query: &value})
	require.NoError(t, err)
	ids, err := photoSetMemberIDs(ctx, s.db, copy.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{b.ID}, ids)
	_, err = s.SetPhotoAssetHidden(ctx, b.ID, b.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	first, err := s.ListPhotoAssets(unlocked, PhotoBrowseRequest{Query: value, Hidden: true, PageSize: 1}, nil)
	require.NoError(t, err)
	require.NotNil(t, first.Next)
	_, err = s.ListPhotoAssets(unlocked, PhotoBrowseRequest{Query: value, PageSize: 1}, first.Next)
	require.ErrorIs(t, err, ErrInvalidPhotoCursor)
	require.NoError(t, s.LockPhotoHidden(ctx))
	_, err = s.ListPhotoAssets(unlocked, PhotoBrowseRequest{Query: value, Hidden: true, PageSize: 1}, first.Next)
	require.ErrorIs(t, err, ErrHiddenLocked)
}

func TestPhotoHiddenConcurrentResetCannotResurrectSession(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	start := make(chan struct{})
	var wait sync.WaitGroup
	var unlockErr, resetErr error
	wait.Go(func() { <-start; _, _, unlockErr = s.UnlockPhotoHidden(ctx, "correct") })
	wait.Go(func() { <-start; resetErr = s.ResetPhotoHidden(ctx) })
	close(start)
	wait.Wait()
	require.NoError(t, resetErr)
	if unlockErr != nil {
		require.ErrorIs(t, unlockErr, ErrHiddenNotConfigured)
	}
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_hidden_sessions`).Scan(&count))
	require.Zero(t, count)
}

func TestPhotoHiddenAuditRestartAndDurableLockout(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "private.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, err = s.initializeAuditAuthority(ctx, s.RootID(), "api", nil)
	require.NoError(t, err)
	reopen := func() {
		path, driver := s.path, s.driver
		require.NoError(t, s.Close())
		s, err = Open(path, driver)
		require.NoError(t, err)
	}
	reopen()
	defer func() { require.NoError(t, s.Close()) }()
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	_, err = s.PhotoAssetByID(WithPhotoHiddenToken(ctx, token), asset.ID)
	require.NoError(t, err)
	require.NoError(t, s.LockPhotoHidden(ctx))
	_, err = s.PhotoAssetByID(WithPhotoHiddenToken(ctx, token), asset.ID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	for range 4 {
		_, _, err = s.UnlockPhotoHidden(ctx, "wrong")
		require.ErrorIs(t, err, ErrHiddenPasscode)
	}
	reopen()
	_, _, err = s.UnlockPhotoHidden(ctx, "wrong")
	var lockout *HiddenLockoutError
	require.ErrorAs(t, err, &lockout)
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	require.Contains(t, backup.String(), "photo_hidden_lockout")
	_, err = s.VerifyAudit(ctx, nil)
	require.NoError(t, err)
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, &backup))
	_, _, err = target.UnlockPhotoHidden(ctx, "correct")
	require.ErrorAs(t, err, &lockout)
}

func TestPhotoHiddenMutationGateAndPrivacyStamp(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "private.jpg")
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	asset, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	state, err := s.PhotoHiddenState(ctx)
	require.NoError(t, err)
	var receipts int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_change_receipts`).Scan(&receipts))
	mutations := []func() (PhotoAsset, error){
		func() (PhotoAsset, error) { return s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, false) },
		func() (PhotoAsset, error) { return s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true) },
		func() (PhotoAsset, error) { return s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true) },
		func() (PhotoAsset, error) {
			return s.PromotePhotoNode(ctx, asset.Files[0].NodeID, &asset.Revision, "", "")
		},
	}
	for _, mutate := range mutations {
		_, err := mutate()
		require.ErrorIs(t, err, ErrHiddenLocked)
	}
	var afterReceipts int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_change_receipts`).Scan(&afterReceipts))
	require.Equal(t, receipts, afterReceipts)
	stored, err := photoAssetByIDQuery(ctx, s.db, asset.ID)
	require.NoError(t, err)
	require.Equal(t, asset, stored)
	_, err = s.CreatePhotoSet(ctx, "Ordinary album edit")
	require.NoError(t, err)
	after, err := s.PhotoHiddenState(ctx)
	require.NoError(t, err)
	require.Equal(t, state.ChangeID, after.ChangeID)
}

func TestPhotoHiddenCoverResponseProjection(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	a := albumAsset(t, s, "private.jpg")
	b := albumAsset(t, s, "visible.jpg")
	album, err := s.CreatePhotoSet(ctx, "Synthetic")
	require.NoError(t, err)
	album, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{a.ID, b.ID}})
	require.NoError(t, err)
	cover := &a.ID
	album, err = s.UpdatePhotoSet(ctx, album.ID, album.Revision, nil, nil, &cover)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	_, err = s.SetPhotoAssetHidden(ctx, a.ID, a.Revision, true)
	require.NoError(t, err)
	summary, err := s.PhotoSet(ctx, album.ID, "")
	require.NoError(t, err)
	require.Nil(t, summary.CoverAssetID)
	list, err := s.ListPhotoSets(ctx, "")
	require.NoError(t, err)
	require.Nil(t, list[0].CoverAssetID)
	album, err = s.UpdatePhotoSet(ctx, album.ID, album.Revision, nil, nil, nil)
	require.NoError(t, err)
	require.Nil(t, album.CoverAssetID)
	star := true
	album, err = s.UpdatePhotoSet(ctx, album.ID, album.Revision, nil, &star, nil)
	require.NoError(t, err)
	require.Nil(t, album.CoverAssetID)
	copy, err := s.DuplicatePhotoSet(ctx, album.ID, album.Revision, "Copy")
	require.NoError(t, err)
	require.Nil(t, copy.CoverAssetID)
	album, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{b.ID}})
	require.NoError(t, err)
	require.Nil(t, album.CoverAssetID)
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	summary, err = s.PhotoSet(WithPhotoHiddenToken(ctx, token), album.ID, "")
	require.NoError(t, err)
	require.Equal(t, &a.ID, summary.CoverAssetID)
}
