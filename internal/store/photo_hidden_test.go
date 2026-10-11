package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func TestPhotoHiddenLifecycle(t *testing.T) {
	t.Parallel()
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
	unchanged, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, false)
	require.NoError(t, err)
	require.Equal(t, asset, unchanged)
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
	var receiptsBefore, receiptsAfter int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts`).Scan(&receiptsBefore))
	unchanged, err = s.SetPhotoAssetHidden(unlocked, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.Equal(t, asset, unchanged)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts`).Scan(&receiptsAfter))
	require.Equal(t, receiptsBefore, receiptsAfter)
	_, err = s.PhotoAssetByID(unlocked, asset.ID)
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(unlocked, node.ID)
	require.NoError(t, err)
	page, err = s.ListPhotoAssets(unlocked, PhotoBrowseRequest{Query: value, Hidden: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), page.Total)
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

func TestPhotoHiddenPasscodeNormalization(t *testing.T) {
	t.Parallel()
	for _, passcode := range []string{"caf\u00e9", "cafe\u0301"} {
		t.Run(passcode, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			require.NoError(t, s.SetupPhotoHidden(ctx, passcode))
			_, _, err := s.UnlockPhotoHidden(ctx, "caf\u00e9")
			require.NoError(t, err)
			_, _, err = s.UnlockPhotoHidden(ctx, "cafe\u0301")
			require.NoError(t, err)
			require.NoError(t, s.ChangePhotoHidden(ctx, "cafe\u0301", "re\u0301placement"))
			_, _, err = s.UnlockPhotoHidden(ctx, "r\u00e9placement")
			require.NoError(t, err)
			_, _, err = s.UnlockPhotoHidden(ctx, "caf\u00e9")
			require.ErrorIs(t, err, ErrHiddenPasscode)
			require.NoError(t, s.DisablePhotoHidden(ctx, "r\u00e9placement"))
		})
	}
}

func TestPhotoHiddenTrashListsFilesAndRestorePreservesHidden(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset, nodes := photoTrashFixture(t, s)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	asset, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	for _, node := range nodes {
		_, _, err = s.Trash(ctx, node.ID, node.Revision)
		require.NoError(t, err)
	}
	page, total, err := s.TrashedRootsPage(ctx, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, page, 1)
	assert.Equal(t, nodes[0].Name, page[0].Name)
	assert.Equal(t, asset.ID, page[0].PhotoAssetID)
	assert.Equal(t, len(nodes), page[0].PhotoFileCount)
	_, _, err = s.Restore(ctx, page[0].ID, page[0].Revision)
	require.NoError(t, err)
	for _, node := range nodes {
		restored, err := s.NodeByID(ctx, node.ID)
		require.NoError(t, err)
		assert.Nil(t, restored.TrashedAt)
	}
	current, err := photoAssetByIDQuery(ctx, s.db, asset.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.HiddenAt, current.HiddenAt)
	_, err = s.PhotoAssetByID(ctx, asset.ID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoHiddenChangeRevokesAndFailedDisableIsAtomic(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	for _, passcode := range []string{"", strings.Repeat("x", 1025)} {
		require.ErrorIs(t, validHiddenPasscode(passcode), ErrInvalidHiddenPasscode)
	}
	for _, hash := range []string{
		"argon2id$m=19456,t=2,p=1$AA$AA",
		"argon2id$m=999999,t=2,p=1$AAECAwQFBgcICQoLDA0ODw$AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
	} {
		_, _, err := hiddenHashParts(hash)
		require.Error(t, err)
	}
	assert.Empty(t, hiddenTokenDigest("AAAA"))
	assert.Empty(t, hiddenTokenDigest(strings.Repeat("x", 1000)))
	_, _, err := newTestStore(t).UnlockPhotoHidden(context.Background(), "")
	require.ErrorIs(t, err, ErrInvalidHiddenPasscode)
}

func TestPhotoHiddenAlbumPopulationAndCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a := albumAsset(t, s, "first.jpg")
	b := albumAsset(t, s, "second.jpg")
	set, err := s.CreatePhotoSet(ctx, "Synthetic album")
	require.NoError(t, err)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{a.ID, b.ID}})
	require.NoError(t, err)
	cover := &a.ID
	set, err = s.UpdatePhotoSet(ctx, set.ID, set.Revision, nil, nil, &cover)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	_, err = s.SetPhotoAssetHidden(ctx, a.ID, a.Revision, true)
	require.NoError(t, err)
	summary, err := s.PhotoSet(ctx, set.ID, "")
	require.NoError(t, err)
	assert.Equal(t, int64(2), summary.MemberCount)
	assert.Equal(t, int64(1), summary.IncludedCount)
	assert.Equal(t, int64(1), summary.HiddenCount)
	require.Nil(t, summary.CoverAssetID)
	list, err := s.ListPhotoSets(ctx, "")
	require.NoError(t, err)
	require.Nil(t, list[0].CoverAssetID)
	set, err = s.UpdatePhotoSet(ctx, set.ID, set.Revision, nil, nil, nil)
	require.NoError(t, err)
	require.Nil(t, set.CoverAssetID)
	star := true
	set, err = s.UpdatePhotoSet(ctx, set.ID, set.Revision, nil, &star, nil)
	require.NoError(t, err)
	require.Nil(t, set.CoverAssetID)
	copied, err := s.DuplicatePhotoSet(ctx, set.ID, set.Revision, "Copy")
	require.NoError(t, err)
	require.Nil(t, copied.CoverAssetID)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{b.ID}})
	require.NoError(t, err)
	require.Nil(t, set.CoverAssetID)
	visible := browsePhotoPage(t, s, `{"filters":{"set_ids":["`+set.ID+`"]},"sort":{"field":"name","direction":"asc"}}`)
	require.Len(t, visible.Items, 1)
	assert.Equal(t, b.ID, visible.Items[0].AssetID)
	duplicate, err := s.CreatePhotoSet(ctx, "Selected query")
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{}`)
	duplicate, err = s.ChangePhotoSetMembers(ctx, duplicate.ID, duplicate.Revision, true, PhotoSetSelection{Query: &value})
	require.NoError(t, err)
	ids, err := photoSetMemberIDs(ctx, s.db, duplicate.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{b.ID}, ids)
	_, err = s.SetPhotoAssetHidden(ctx, b.ID, b.Revision, true)
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	summary, err = s.PhotoSet(unlocked, set.ID, "")
	require.NoError(t, err)
	require.Equal(t, &a.ID, summary.CoverAssetID)
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
	t.Parallel()
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
	require.Empty(t, s.photoHiddenSessions)
}

func TestPhotoHiddenAuditRestartAndDurableLockout(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	s.photoHiddenNow = func() time.Time { return now }
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
		s.photoHiddenNow = func() time.Time { return now }
	}
	reopen()
	defer func() { require.NoError(t, s.Close()) }()
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	_, err = s.PhotoAssetByID(WithPhotoHiddenToken(ctx, token), asset.ID)
	require.NoError(t, err)
	reopen()
	_, err = s.PhotoAssetByID(WithPhotoHiddenToken(ctx, token), asset.ID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err = s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	require.NoError(t, s.LockPhotoHidden(ctx))
	_, err = s.PhotoAssetByID(WithPhotoHiddenToken(ctx, token), asset.ID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	for range 4 {
		_, _, err = s.UnlockPhotoHidden(ctx, "wrong")
		require.ErrorIs(t, err, ErrHiddenPasscode)
	}
	reopen()
	var lockout *HiddenLockoutError
	_, _, err = s.UnlockPhotoHidden(ctx, "wrong")
	require.ErrorAs(t, err, &lockout)
	reopen()
	_, _, err = s.UnlockPhotoHidden(ctx, "correct")
	require.ErrorAs(t, err, &lockout)
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	require.NotContains(t, backup.String(), "photo_hidden_lockout")
	require.NotContains(t, backup.String(), "photo_hidden_failure")
	require.Contains(t, backup.String(), "photo_hidden_credential")
	require.Contains(t, backup.String(), `"hidden_at"`)
	require.Contains(t, backup.String(), `"operation":"hide"`)
	require.NotContains(t, backup.String(), token)
	require.NotContains(t, backup.String(), hiddenTokenDigest(token))
	require.NotContains(t, backup.String(), "photo_hidden_session")
	_, err = s.VerifyAudit(ctx, nil)
	require.NoError(t, err)
	target := newTestStore(t)
	target.photoHiddenNow = func() time.Time { return now }
	require.NoError(t, target.ImportMetadata(ctx, &backup))
	var attempts int
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM photo_hidden_failures`).Scan(&attempts))
	require.Zero(t, attempts)
	_, err = target.PhotoAssetByID(WithPhotoHiddenToken(ctx, token), asset.ID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	restoredToken, _, err := target.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	restored, err := target.PhotoAssetByID(WithPhotoHiddenToken(ctx, restoredToken), asset.ID)
	require.NoError(t, err)
	require.NotNil(t, restored.HiddenAt)
}

func TestPhotoHiddenMutationGate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "private.jpg")
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	asset, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	var receipts int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_change_receipts`).Scan(&receipts))
	mutations := []func() (PhotoAsset, error){
		func() (PhotoAsset, error) { return s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, false) },
		func() (PhotoAsset, error) { return s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true) },
		func() (PhotoAsset, error) { return s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true) },
		func() (PhotoAsset, error) { return s.TrashPhotoAsset(ctx, asset.ID, asset.Revision) },
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
}

func TestPhotoHiddenCoverAndPromoteAuthorization(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "private.jpg")
	target := albumAsset(t, s, "target.jpg")
	checkOwner := func(ctx context.Context, want error) {
		_, err := s.CreatePhotoAsset(ctx, asset.Files[0].NodeID, "", "")
		require.ErrorIs(t, err, want)
		if errors.Is(want, ErrHiddenLocked) {
			require.NotContains(t, err.Error(), asset.ID)
		} else {
			require.ErrorContains(t, err, asset.ID)
		}
		_, err = s.AttachPhotoFile(ctx, target.ID, target.Revision, asset.Files[0].NodeID, "", nil)
		require.ErrorIs(t, err, want)
		if errors.Is(want, ErrHiddenLocked) {
			require.NotContains(t, err.Error(), asset.ID)
		} else {
			require.ErrorContains(t, err, asset.ID)
		}
		unchanged, err := s.PhotoAssetByID(ctx, target.ID)
		require.NoError(t, err)
		require.Equal(t, target, unchanged)
	}
	checkOwner(ctx, ErrPhotoNodeOwned)
	album, err := s.CreatePhotoSet(ctx, "Synthetic")
	require.NoError(t, err)
	album, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	missing := new("00000000-0000-4000-8000-000000000001")
	_, err = s.UpdatePhotoSet(ctx, album.ID, album.Revision, nil, nil, &missing)
	require.ErrorIs(t, err, ErrInvalidPhotoAlbum)
	cover := &target.ID
	_, err = s.UpdatePhotoSet(ctx, album.ID, album.Revision, nil, nil, &cover)
	require.ErrorIs(t, err, ErrInvalidPhotoAlbum)
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	checkOwner(ctx, ErrHiddenLocked)
	cover = &asset.ID
	_, err = s.UpdatePhotoSet(ctx, album.ID, album.Revision, nil, nil, &cover)
	require.ErrorIs(t, err, ErrHiddenLocked)
	other, err := s.CreatePhotoSet(ctx, "Empty")
	require.NoError(t, err)
	_, err = s.UpdatePhotoSet(ctx, other.ID, other.Revision, nil, nil, &cover)
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	checkOwner(unlocked, ErrPhotoNodeOwned)
	_, err = s.UpdatePhotoSet(unlocked, other.ID, other.Revision, nil, nil, &cover)
	require.ErrorIs(t, err, ErrInvalidPhotoAlbum)
	_, err = s.PromotePhotoNode(ctx, asset.Files[0].NodeID, nil, "", "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	s.photoHiddenSessions[hiddenTokenDigest(token)] = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	checkOwner(unlocked, ErrHiddenLocked)
	token, _, err = s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	unlocked = WithPhotoHiddenToken(ctx, token)
	_, err = s.PromotePhotoNode(unlocked, asset.Files[0].NodeID, nil, "", "")
	require.ErrorIs(t, err, ErrStaleRevision)
	require.ErrorContains(t, err, "needs its revision")
	_, err = s.UpdatePhotoSet(unlocked, album.ID, album.Revision, nil, nil, &cover)
	require.NoError(t, err)
	asset, err = s.SetPhotoAssetHidden(unlocked, asset.ID, asset.Revision, false)
	require.NoError(t, err)
}
