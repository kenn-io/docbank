package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

// BenchmarkPhotoXMPStatus100k measures idle worker listing and 250-row
// xmp:pending and xmp:conflict pages over 100,000 photos, of which 1,000 are
// Pending and 1,000 conflict. Seeding runs outside the timer; run it with
// go test -tags fts5 -run '^$' -bench PhotoXMPStatus100k ./internal/store/.
func BenchmarkPhotoXMPStatus100k(b *testing.B) {
	const photos = 100_000
	s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	ctx := b.Context()
	started := time.Now()
	for batch := 0; batch < photos; batch += 10_000 {
		require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
			for i := batch; i < batch+10_000; i++ {
				node, _, err := s.createFileTx(ctx, tx, s.RootID(), fmt.Sprintf("photo-%06d.jpg", i), fmt.Sprintf("%064x", i+1), 128, "image/jpeg")
				if err != nil {
					return err
				}
				if _, err := s.insertPhotoAssetTx(ctx, tx, node.ID, PhotoRoleImage, PhotoKindPhoto); err != nil {
					return err
				}
			}
			return nil
		}))
	}
	_, err = s.db.ExecContext(ctx, `UPDATE photo_files SET xmp_state=CASE WHEN node_id%100=0 THEN 'pending' WHEN node_id%100=1 THEN 'conflict' ELSE 'synced' END`)
	require.NoError(b, err)
	b.Logf("seeded %d photos in %s", photos, time.Since(started).Round(time.Millisecond))
	page := func(b *testing.B, text string) {
		b.Helper()
		result, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{PageSize: 250, Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: text, Sort: query.Sort{Field: "name", Direction: "asc"}}}, nil)
		require.NoError(b, err)
		require.Len(b, result.Items, 250)
	}
	b.Run("idle worker listing", func(b *testing.B) {
		_, err := s.db.ExecContext(ctx, `UPDATE photo_files SET xmp_state='synced' WHERE xmp_state='pending'`)
		require.NoError(b, err)
		b.Cleanup(func() {
			_, err := s.db.ExecContext(ctx, `UPDATE photo_files SET xmp_state='pending' WHERE node_id%100=0`)
			require.NoError(b, err)
		})
		for b.Loop() {
			targets, err := s.PhotoXMPTargetsAfter(ctx, "", 10)
			require.NoError(b, err)
			require.Empty(b, targets)
		}
	})
	b.Run("pending page", func(b *testing.B) {
		for b.Loop() {
			page(b, "xmp:pending")
		}
	})
	b.Run("conflict page", func(b *testing.B) {
		for b.Loop() {
			page(b, "xmp:conflict")
		}
	})
}
