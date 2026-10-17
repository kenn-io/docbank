package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
)

func nextPhotoEvent(t *testing.T, ch <-chan PhotoEvent) PhotoEvent {
	t.Helper()
	select {
	case e := <-ch:
		return e
	default:
		t.Fatal("missing committed photo event")
		return PhotoEvent{}
	}
}
func noPhotoEvent(t *testing.T, ch <-chan PhotoEvent) {
	t.Helper()
	select {
	case e := <-ch:
		t.Fatalf("unexpected event: %+v", e)
	default:
	}
}
func TestPhotoEventsDeliveryAndRollback(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	node, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, ch))
	rejected := errors.New("rollback")
	require.ErrorIs(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, asset.ID)
		require.NoError(t, err)
		return rejected
	}), rejected)
	noPhotoEvent(t, ch)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, asset.ID)
		return err
	}))
	event := nextPhotoEvent(t, ch)
	locked, err := s.VisiblePhotoEvent(ctx, event)
	require.NoError(t, err)
	require.Empty(t, locked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	for _, viewer := range []context.Context{ctx, WithPhotoHiddenToken(ctx, token)} {
		visible, err := s.VisiblePhotoEvent(viewer, event)
		require.NoError(t, err)
		if viewer == ctx {
			require.Equal(t, PhotoEvent{Refetch: true}, visible)
		} else {
			require.Equal(t, asset.ID, visible.AssetID)
			require.Equal(t, asset.Revision+1, visible.Revision)
		}
	}
	visible, err := s.VisiblePhotoEvent(ctx, PhotoEvent{AssetID: "missing"})
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, visible)
}
func TestPhotoEventsArrivalAndUnlockDuringWrite(t *testing.T) {
	t.Parallel()
	for _, hidden := range []bool{false, true} {
		t.Run(map[bool]string{false: "arrival", true: "unlock"}[hidden], func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
			node, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			if hidden {
				asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
			}
			token, expiry, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
			require.NoError(t, err)
			s.photoHiddenSessionsMu.Lock()
			delete(s.photoHiddenSessions, hiddenTokenDigest(token))
			s.photoHiddenSessionsMu.Unlock()
			var ch <-chan PhotoEvent
			stop := func() {}
			if hidden {
				ch, stop = s.SubscribePhotoEvents()
			}
			defer func() { stop() }()
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				done <- s.withStorageTx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, asset.ID)
					close(started)
					<-release
					return err
				})
			}()
			<-started
			viewer := ctx
			if hidden {
				// Unlock commits before installing its session. Reproduce that insertion gap
				// directly: a second public unlock would wait for this held writer.
				s.photoHiddenSessionsMu.Lock()
				s.photoHiddenSessions[hiddenTokenDigest(token)] = expiry
				s.photoHiddenSessionsMu.Unlock()
				viewer = WithPhotoHiddenToken(ctx, token)
			} else {
				ch, stop = s.SubscribePhotoEvents()
			}
			close(release)
			require.NoError(t, <-done)
			event, err := s.VisiblePhotoEvent(viewer, nextPhotoEvent(t, ch))
			require.NoError(t, err)
			if hidden {
				require.Equal(t, asset.ID, event.AssetID)
				require.Equal(t, asset.Revision+1, event.Revision)
			} else {
				require.Equal(t, PhotoEvent{Refetch: true}, event)
			}
		})
	}
}
func TestPhotoEventsOverflow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	events := make([]PhotoEvent, 65)
	for i := range events {
		events[i] = PhotoEvent{AssetID: "photo"}
	}
	s.publishPhotoChanges(events)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, ch))
	noPhotoEvent(t, ch)
}
