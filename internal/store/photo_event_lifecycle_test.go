package store

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func photoCaptureObjects(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	require.NoError(t, s.writeDB.QueryRowContext(t.Context(), `SELECT count(*) FROM temp.sqlite_schema WHERE name LIKE 'docbank_photo_%' OR name LIKE 'photo_events_%'`).Scan(&count))
	return count
}

func TestPhotoEventsCaptureInstallAndUninstall(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "capture.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	require.Zero(t, photoCaptureObjects(t, s))
	events, cancel := s.SubscribePhotoEvents()
	_, other := s.SubscribePhotoEvents()
	node, _, err = s.Move(ctx, node.ID, s.RootID(), "renamed.jpg", node.Revision)
	require.NoError(t, err)
	require.NotEmpty(t, nextPhotoEvent(t, events).AssetID)
	require.Positive(t, photoCaptureObjects(t, s))
	other()
	require.Positive(t, photoCaptureObjects(t, s))
	cancel()
	require.Zero(t, photoCaptureObjects(t, s))
	_, _, err = s.Move(ctx, node.ID, s.RootID(), "idle.jpg", node.Revision)
	require.NoError(t, err)
	require.Zero(t, photoCaptureObjects(t, s))
	_, cancel = s.SubscribePhotoEvents()
	require.NoError(t, s.withStorageTx(t.Context(), func(*sql.Tx) error { cancel(); return nil }))
	require.Positive(t, photoCaptureObjects(t, s))
	aborted := errors.New("synthetic write failure")
	require.ErrorIs(t, s.withStorageTx(t.Context(), func(*sql.Tx) error { return aborted }), aborted)
	require.Positive(t, photoCaptureObjects(t, s))
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var count int
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM temp.sqlite_schema WHERE name LIKE 'docbank_photo_%' OR name LIKE 'photo_events_%'`).Scan(&count))
		require.Zero(t, count)
		_, err := insertPhotoSet(t.Context(), tx, "after disconnect")
		return err
	}))
}

func TestPhotoEventsPartialInstallRollbackAndRetry(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	s.writeDB.SetMaxIdleConns(0)
	s.writeDB.SetMaxIdleConns(1)
	_, err := s.writeDB.ExecContext(ctx, `CREATE TEMP TRIGGER photo_events_nodes_INSERT AFTER INSERT ON main.nodes BEGIN SELECT 1; END`)
	require.NoError(t, err)
	events, cancel := s.SubscribePhotoEvents()
	defer cancel()
	node, err := s.CreateFile(ctx, s.RootID(), "capture.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{Refetch: true}, nextPhotoEvent(t, events))
	_, err = s.writeDB.ExecContext(ctx, `DROP TRIGGER photo_events_nodes_INSERT`)
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	require.Equal(t, PhotoEvent{AssetID: asset.ID}, nextPhotoEvent(t, events))
}

func TestPhotoEventsSubscriptionRacesWithWrites(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"uncaptured", "resubscribe", "hidden"} {
		t.Run(mode, func(t *testing.T) {
			// Loaded machines can take seconds per step; the test timeout bounds a hang.
			wait := func(done <-chan struct{}) {
				t.Helper()
				<-done
			}
			cancelSubscription := func(cancel func()) {
				t.Helper()
				cancelled := make(chan struct{})
				go func() { cancel(); close(cancelled) }()
				wait(cancelled)
			}
			s := newTestStore(t)
			ctx := t.Context()
			var assetID string
			if mode == "hidden" {
				require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
				node, err := s.CreateFile(ctx, s.RootID(), "hidden.jpg", fakeHash("11"), 4, "image/jpeg")
				require.NoError(t, err)
				asset, err := s.PhotoAssetForNode(ctx, node.ID)
				require.NoError(t, err)
				_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
				assetID = asset.ID
				_, _, err = s.UnlockPhotoHidden(ctx, "synthetic-passcode")
				require.NoError(t, err)
			}
			var prior <-chan PhotoEvent
			var cancelPrior func()
			if mode != "uncaptured" {
				prior, cancelPrior = s.SubscribePhotoEvents()
				t.Cleanup(func() { cancelSubscription(cancelPrior) })
				if mode == "resubscribe" {
					require.NoError(t, s.withStorageTx(ctx, func(*sql.Tx) error { return nil }))
				}
			}
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var releaseOnce sync.Once
			releaseWriter := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseWriter)
			go func() {
				done <- s.withStorageTx(ctx, func(tx *sql.Tx) error {
					var err error
					if mode == "hidden" {
						_, err = tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, assetID)
					} else {
						_, err = insertPhotoSet(ctx, tx, "concurrent")
					}
					close(started)
					<-release
					return err
				})
			}()
			wait(started)
			var cancelled chan struct{}
			if mode == "resubscribe" {
				cancelled = make(chan struct{})
				go func() { cancelPrior(); close(cancelled) }()
				drained := make(chan struct{})
				go func() {
					for range prior {
					}
					close(drained)
				}()
				wait(drained)
			}
			type subscription struct {
				events <-chan PhotoEvent
				cancel func()
			}
			subscribed := make(chan subscription, 1)
			go func() {
				events, cancel := s.SubscribePhotoEvents()
				subscribed <- subscription{events, cancel}
			}()
			active := <-subscribed
			t.Cleanup(func() {
				releaseWriter()
				cancelSubscription(active.cancel)
			})
			events := active.events
			releaseWriter()
			require.NoError(t, <-done)
			event := nextPhotoEventWithAudience(t, events)
			switch mode {
			case "uncaptured":
				event.audience = 0
				require.Equal(t, PhotoEvent{Refetch: true}, event)
			case "resubscribe":
				wait(cancelled)
				require.NotEmpty(t, event.AlbumID)
			case "hidden":
				require.Equal(t, assetID, event.AssetID)
				require.False(t, event.Refetch)
				require.Equal(t, photoHidden, event.audience)
				priorEvent := nextPhotoEventWithAudience(t, prior)
				require.Equal(t, assetID, priorEvent.AssetID)
				require.Equal(t, photoHidden, priorEvent.audience)
			}
		})
	}
}

func TestPhotoEventsStoreShutdownClosesSubscriptions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	shutdownEvents, shutdownCancel := s.SubscribePhotoEvents()
	defer shutdownCancel()
	require.NoError(t, s.Close())
	select {
	case _, ok := <-shutdownEvents:
		require.False(t, ok)
	case <-time.After(3 * time.Second):
		t.Fatal("photo event subscription did not close on store shutdown")
	}
	later, unsubscribe := s.SubscribePhotoEvents()
	defer unsubscribe()
	select {
	case _, ok := <-later:
		require.False(t, ok)
	case <-time.After(3 * time.Second):
		t.Fatal("photo event subscription created after shutdown did not close")
	}
}
