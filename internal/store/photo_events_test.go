package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func nextPhotoEvent(t *testing.T, ch <-chan []PhotoEvent) PhotoEvent {
	t.Helper()
	select {
	case e := <-ch:
		require.Len(t, e, 1)
		return e[0]
	default:
		t.Fatal("missing committed photo event")
		return PhotoEvent{}
	}
}
func noPhotoEvent(t *testing.T, ch <-chan []PhotoEvent) {
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
	locked, err := s.VisiblePhotoEvents(ctx, []PhotoEvent{event})
	require.NoError(t, err)
	require.Empty(t, locked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	for _, viewer := range []context.Context{ctx, WithPhotoHiddenToken(ctx, token)} {
		visible, err := s.VisiblePhotoEvents(viewer, []PhotoEvent{event})
		require.NoError(t, err)
		if viewer == ctx {
			require.Equal(t, []PhotoEvent{{Refetch: true}}, visible)
		} else {
			require.Equal(t, asset.ID, visible[0].AssetID)
			require.Equal(t, asset.Revision+1, visible[0].Revision)
		}
	}
	visible, err := s.VisiblePhotoEvents(ctx, []PhotoEvent{{AssetID: "missing"}})
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{Refetch: true}}, visible)
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
			var ch <-chan []PhotoEvent
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
			event, err := s.VisiblePhotoEvents(viewer, []PhotoEvent{nextPhotoEvent(t, ch)})
			require.NoError(t, err)
			if hidden {
				require.Equal(t, asset.ID, event[0].AssetID)
				require.Equal(t, asset.Revision+1, event[0].Revision)
			} else {
				require.Equal(t, []PhotoEvent{{Refetch: true}}, event)
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

func TestPhotoEventsPollCursors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	s.photoHiddenNow = func() time.Time { return now }
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	node, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	cursor := func(viewer context.Context) string {
		t.Helper()
		state, err := s.PhotoHiddenState(viewer)
		require.NoError(t, err)
		value, err := s.PhotoChangeCursor(viewer)
		require.NoError(t, err)
		require.Equal(t, value, state.ChangeCursor)
		require.Equal(t, state.ExpiresAt != nil, strings.Contains(state.ChangeCursor, ":hidden:"))
		return state.ChangeCursor
	}
	write := func() {
		t.Helper()
		require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, asset.ID)
			return err
		}))
	}
	locked := cursor(ctx)
	require.Equal(t, locked, cursor(ctx))
	write()
	require.Equal(t, locked, cursor(ctx), "Hidden-only writes stay silent without an unlocked viewer")
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	viewer := WithPhotoHiddenToken(ctx, token)
	unlocked := cursor(viewer)
	write()
	require.NotEqual(t, locked, cursor(ctx), "locked holders receive the bare wakeup for unlocked followers")
	require.NotEqual(t, unlocked, cursor(viewer))
	locked = cursor(ctx)
	rejected := errors.New("rollback")
	require.ErrorIs(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, asset.ID)
		require.NoError(t, err)
		return rejected
	}), rejected)
	require.Equal(t, locked, cursor(ctx))
	_, err = s.CreatePhotoSet(ctx, "Album")
	require.NoError(t, err)
	require.NotEqual(t, locked, cursor(ctx))
	locked = cursor(ctx)
	now = now.Add(31 * time.Second)
	write() // No subscriber or poll interest remains.
	require.NotEqual(t, locked, cursor(ctx), "resuming expired interest must refetch uncaptured changes")
	now = now.Add(5 * time.Minute)
	require.Equal(t, cursor(ctx), cursor(viewer), "expired unlock uses the locked audience")
	other := newTestStore(t)
	restarted, err := other.PhotoChangeCursor(ctx)
	require.NoError(t, err)
	require.NotEqual(t, cursor(ctx), restarted)
}

func TestPhotoEventsPollStartsDuringWrite(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.withStorageTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1 WHERE asset_id=(SELECT asset_id FROM photo_files WHERE node_id=?)`, node.ID)
			close(started)
			<-release
			return err
		})
	}()
	<-started
	before, err := s.PhotoChangeCursor(ctx)
	close(release)
	require.NoError(t, err)
	require.NoError(t, <-done)
	after, err := s.PhotoChangeCursor(ctx)
	require.NoError(t, err)
	require.NotEqual(t, before, after)
}

func TestPhotoEventsBatchVisibility(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	visibleNode, err := s.CreateFile(ctx, s.RootID(), "visible.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	visible, err := s.PhotoAssetForNode(ctx, visibleNode.ID)
	require.NoError(t, err)
	hiddenNode, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", fakeHash("22"), 4, "image/jpeg")
	require.NoError(t, err)
	hidden, err := s.PhotoAssetForNode(ctx, hiddenNode.ID)
	require.NoError(t, err)
	hidden, err = s.SetPhotoAssetHidden(ctx, hidden.ID, hidden.Revision, true)
	require.NoError(t, err)
	batch := []PhotoEvent{{AssetID: visible.ID, Revision: -1}, {AssetID: hidden.ID, Revision: -1}, {AssetID: "missing"}}
	events, err := s.VisiblePhotoEvents(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: visible.ID, Revision: visible.Revision}, {Refetch: true}}, events)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	events, err = s.VisiblePhotoEvents(WithPhotoHiddenToken(ctx, token), batch)
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: visible.ID, Revision: visible.Revision}, {AssetID: hidden.ID, Revision: hidden.Revision}, {Refetch: true}}, events)
	events, err = s.VisiblePhotoEvents(ctx, batch[:2])
	require.NoError(t, err)
	require.Equal(t, []PhotoEvent{{AssetID: visible.ID, Revision: visible.Revision}, {Refetch: true}}, events)
}

func TestPhotoEventsKeepCommittedBatch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	var want []PhotoEvent
	for _, name := range []string{"first.jpg", "second.jpg"} {
		node, err := s.CreateFile(ctx, s.RootID(), name, fakeHash("11"), 4, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		want = append(want, PhotoEvent{AssetID: asset.ID, Revision: asset.Revision + 1})
	}
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1`)
		return err
	}))
	select {
	case batch := <-ch:
		require.ElementsMatch(t, want, batch)
		visible, err := s.VisiblePhotoEvents(ctx, batch)
		require.NoError(t, err)
		require.ElementsMatch(t, want, visible)
	default:
		t.Fatal("missing committed batch")
	}
	noPhotoEvent(t, ch)
}
