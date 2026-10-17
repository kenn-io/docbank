package store

import (
	"database/sql"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPhotoEventsReleaseWriterBeforeResolution(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	s.db.SetMaxOpenConns(1)
	read, err := s.db.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = read.Close() }()
	events, stop := s.SubscribePhotoEvents()
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- s.withStorageTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, asset.ID)
			return err
		})
	}()
	require.Eventually(t, func() bool {
		var revision int64
		return read.QueryRowContext(ctx, `SELECT revision FROM photo_assets WHERE asset_id=?`, asset.ID).Scan(&revision) == nil && revision > asset.Revision
	}, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return s.writeDB.Stats().InUse == 0 }, time.Second, time.Millisecond,
		"a committed transaction must release the writer while event reads wait")
	require.NoError(t, read.Close())
	require.NoError(t, <-done)
	require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
}

func TestPhotoEventsCaptureFlagAndWriterReplacement(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "capture.jpg", fakeHash("11"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	// Subscription churn retains the same temporary objects and clears the old batch.
	for _, name := range []string{"first.jpg", "second.jpg"} {
		events, cancel := s.SubscribePhotoEvents()
		node, _, err = s.Move(ctx, node.ID, s.RootID(), name, node.Revision)
		require.NoError(t, err)
		require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
		cancel()
		// This transaction begins with no subscribers. Its mutation must add no keys.
		require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM docbank_photo_changes; UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, asset.ID); err != nil {
				return err
			}
			var count int
			require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM docbank_photo_changes`).Scan(&count))
			require.Zero(t, count)
			return nil
		}))
	}
	events, cancel := s.SubscribePhotoEvents()
	defer cancel()
	s.writeDB.SetMaxIdleConns(0)
	s.writeDB.SetMaxIdleConns(1)
	_, _, err = s.Move(ctx, node.ID, s.RootID(), "replacement-writer.jpg", node.Revision)
	require.NoError(t, err)
	require.Equal(t, asset.ID, nextPhotoEvent(t, events).AssetID)
	noPhotoEvent(t, events)
}
