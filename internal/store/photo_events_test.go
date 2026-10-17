package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/stretchr/testify/require"
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
}
func TestPhotoEventsArrivalUnlockAndPollDuringWrite(t *testing.T) {
	t.Parallel()
	for _, interest := range []string{"arrival", "unlock", "poll"} {
		t.Run(interest, func(t *testing.T) {
			hidden := interest == "unlock"
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
			var token string
			var expiry time.Time
			if hidden {
				token, expiry, err = s.UnlockPhotoHidden(ctx, "synthetic-passcode")
				require.NoError(t, err)
				s.photoHiddenSessionsMu.Lock()
				delete(s.photoHiddenSessions, hiddenTokenDigest(token))
				s.photoHiddenSessionsMu.Unlock()
			}
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
			if interest == "poll" {
				before, err := s.PhotoHiddenState(ctx, true)
				close(release)
				require.NoError(t, err)
				require.NoError(t, <-done)
				after, err := s.PhotoHiddenState(ctx, true)
				require.NoError(t, err)
				require.NotEqual(t, before.ChangeCursor, after.ChangeCursor)
				return
			}
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
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	ch, stop := s.SubscribePhotoEvents()
	defer stop()
	for i := range 65 {
		asset, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, i%2 == 0)
		require.NoError(t, err)
	}
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, ch))
	noPhotoEvent(t, ch)
	asset, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, false)
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{AssetID: asset.ID, Refetch: true}, nextPhotoEvent(t, ch))
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
		state, err := s.PhotoHiddenState(viewer, true)
		require.NoError(t, err)
		return state.ChangeCursor
	}
	write := func() {
		t.Helper()
		require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, asset.ID)
			return err
		}))
	}
	idle, err := s.PhotoHiddenState(ctx, false)
	require.NoError(t, err)
	_, err = s.CreatePhotoSet(ctx, "Idle album")
	require.NoError(t, err)
	afterIdle, err := s.PhotoHiddenState(ctx, false)
	require.NoError(t, err)
	require.Equal(t, idle.ChangeCursor, afterIdle.ChangeCursor, "ordinary state reads leave capture idle")
	locked := cursor(ctx)
	require.Equal(t, locked, cursor(ctx))
	write()
	require.Equal(t, locked, cursor(ctx), "Hidden-only writes stay silent without an unlocked viewer")
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	viewer := WithPhotoHiddenToken(ctx, token)
	unlocked := cursor(viewer)
	write()
	require.Equal(t, locked, cursor(ctx), "another viewer unlocking does not expose Hidden changes")
	require.NotEqual(t, unlocked, cursor(viewer))
	_, err = s.CreatePhotoSet(ctx, "Album")
	require.NoError(t, err)
	require.NotEqual(t, locked, cursor(ctx))
	now = now.Add(5 * time.Minute)
	require.Equal(t, cursor(ctx), cursor(viewer), "expired unlock uses the locked audience")
	other := newTestStore(t)
	restarted, err := other.PhotoHiddenState(ctx, true)
	require.NoError(t, err)
	require.NotEqual(t, cursor(ctx), restarted.ChangeCursor)
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
	require.Equal(t, []PhotoEvent{{AssetID: visible.ID, Revision: visible.Revision}}, events)
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
		want = append(want, PhotoEvent{AssetID: asset.ID, Refetch: true})
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
	default:
		t.Fatal("missing committed batch")
	}
	noPhotoEvent(t, ch)
}

func TestPhotoEventsStreamDoesNotRenewPollingInterest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	now := time.Now().UTC()
	s.photoHiddenNow = func() time.Time { return now }
	_, err := s.PhotoHiddenState(t.Context(), true)
	require.NoError(t, err)
	_, stop := s.SubscribePhotoEvents()
	defer stop()
	cursor, err := s.PhotoHiddenState(t.Context(), true)
	require.NoError(t, err)
	epoch, lease := s.photoEvents.epoch, s.photoEvents.pollUntil
	now = now.Add(time.Minute)
	current, err := s.PhotoHiddenState(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, cursor.ChangeCursor, current.ChangeCursor)
	require.Equal(t, epoch, s.photoEvents.epoch)
	require.Equal(t, lease, s.photoEvents.pollUntil)
}

func TestPhotoEventsStaggeredPollingAcrossExpiry(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"write before resuming", "write between polls"} {
		t.Run(order, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			now := time.Now().UTC()
			s.photoHiddenNow = func() time.Time { return now }
			node, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			poll := func() string {
				state, err := s.PhotoHiddenState(ctx, true)
				require.NoError(t, err)
				return state.ChangeCursor
			}
			clientA := poll()
			now = now.Add(10 * time.Second)
			clientB := poll()
			require.Equal(t, clientA, clientB)
			now = now.Add(31 * time.Second)
			write := func() {
				_, err := s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
			}
			if order == "write before resuming" {
				write()
			}
			resumedA := poll()
			require.NotEqual(t, clientA, resumedA)
			now = now.Add(time.Second)
			if order == "write between polls" {
				write()
			}
			resumedB := poll()
			require.NotEqual(t, clientB, resumedB)
			now = now.Add(time.Second)
			currentA := poll()
			require.Equal(t, resumedB, currentA)
			if order == "write between polls" {
				require.NotEqual(t, resumedA, currentA)
			}
		})
	}
}
