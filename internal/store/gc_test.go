package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestUnreachableBlobsPageBoundsCandidatesAcrossCatalogSizes(t *testing.T) {
	for _, liveCount := range []int{3, 300} {
		t.Run(fmt.Sprintf("live=%d", liveCount), func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			for i := range liveCount {
				_, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("live-%03d", i),
					fmt.Sprintf("%064x", 1000+i), 1, "application/octet-stream")
				require.NoError(t, err)
			}
			want := []string{
				fmt.Sprintf("%064x", 10), fmt.Sprintf("%064x", 20),
				fmt.Sprintf("%064x", 30), fmt.Sprintf("%064x", 40),
				fmt.Sprintf("%064x", 50), fmt.Sprintf("%064x", 60),
				fmt.Sprintf("%064x", 70),
			}
			for _, hash := range want {
				_, err := s.db.ExecContext(ctx,
					`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
					hash, "2026-01-01T00:00:00Z")
				require.NoError(t, err)
			}

			var got []string
			var after string
			for {
				page, more, err := s.UnreachableBlobsPage(ctx, after, 3)
				require.NoError(t, err)
				require.LessOrEqual(t, len(page), 3)
				for _, candidate := range page {
					got = append(got, candidate.Hash)
				}
				if !more {
					break
				}
				require.NotEmpty(t, page)
				after = page[len(page)-1].Hash
			}
			assert.Equal(t, want, got)
		})
	}
}

func TestBlobPageUsesHashKeysetAcrossDeletionAndLowerInsertion(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash05 := fmt.Sprintf("%064x", 5)
	hash10 := fmt.Sprintf("%064x", 10)
	hash20 := fmt.Sprintf("%064x", 20)
	hash30 := fmt.Sprintf("%064x", 30)
	for _, hash := range []string{hash10, hash20, hash30} {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
			hash, "2026-01-01T00:00:00.000000000Z")
		require.NoError(t, err)
	}

	first, more, err := s.BlobsPage(ctx, "", 1)
	require.NoError(t, err)
	require.True(t, more)
	require.Len(t, first, 1)
	assert.Equal(t, hash10, first[0].Hash)

	require.NoError(t, s.DeleteBlobRows(ctx, []string{hash10, hash20}))
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
		hash05, "2026-01-01T00:00:00Z")
	require.NoError(t, err)

	resumed, more, err := s.BlobsPage(ctx, hash10, 10)
	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, resumed, 1)
	assert.Equal(t, hash30, resumed[0].Hash,
		"a resumed cycle neither revisits deletions nor admits a new lower hash")

	fresh, more, err := s.BlobsPage(ctx, "", 10)
	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, fresh, 2)
	assert.Equal(t, []string{hash05, hash30}, []string{fresh[0].Hash, fresh[1].Hash})
}

func TestSparseRepackPageUsesCanonicalLiveHashKeyset(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	for i, hash := range []string{
		fmt.Sprintf("%064x", 30), fmt.Sprintf("%064x", 10), fmt.Sprintf("%064x", 20),
	} {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
			hash, "2026-01-01T00:00:00Z")
		require.NoError(t, err)
		packID := pack.NewPackID()
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at)
			VALUES (?, 3, 30, ?)`, packID, "2026-01-01T00:00:00.000000000Z")
		require.NoError(t, err)
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO blob_pack_index
				(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
			VALUES (?, ?, ?, 5, 1, 0, 0)`, hash, packID, pack.MinEntryOffset+int64(i)*32)
		require.NoError(t, err)
	}

	first, more, err := s.SparseRepackPage(ctx, "", 2,
		time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC), time.Nanosecond, 1)
	require.NoError(t, err)
	require.True(t, more)
	require.Len(t, first, 2)
	assert.Equal(t, fmt.Sprintf("%064x", 10), first[0].Hash)
	assert.Equal(t, fmt.Sprintf("%064x", 20), first[1].Hash)

	second, more, err := s.SparseRepackPage(ctx, first[1].Hash, 2,
		time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC), time.Nanosecond, 1)
	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, second, 1)
	assert.Equal(t, fmt.Sprintf("%064x", 30), second[0].Hash)
}

func TestDeadPackUsagePageIsBounded(t *testing.T) {
	s := newTestStore(t)
	for range 4 {
		_, err := s.db.ExecContext(t.Context(), `
			INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at)
			VALUES (?, 1, 20, ?)`, pack.NewPackID(), "2026-01-01T00:00:00.000000000Z")
		require.NoError(t, err)
	}

	page, more, err := s.DeadPackUsagePage(t.Context(), 3)
	require.NoError(t, err)
	assert.True(t, more)
	assert.Len(t, page, 3)
}

func TestUnreachableBlobs(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	live, err := s.CreateFile(ctx, s.RootID(), "live.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	trashed, err := s.CreateFile(ctx, s.RootID(), "trashed.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	gone, err := s.CreateFile(ctx, s.RootID(), "gone.txt", fakeHash("c3"), 1, "text/plain")
	require.NoError(t, err)

	// Replacing the current pointer leaves the original version as a root.
	_, err = s.db.Exec(`INSERT INTO blobs (hash, size, created_at) VALUES (?, 9, ?)`,
		fakeHash("d4"), "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	_, err = s.db.Exec(
		`INSERT INTO content_versions (
			version_id, node_id, blob_hash, size, recorded_at, node_revision,
			introduced_operation_id, transition_kind
		) VALUES ('44444444-4444-4444-8444-444444444444', ?, ?, 9, ?, 2,
			'dddddddd-dddd-4ddd-8ddd-dddddddddddd', 'content_replace')`,
		live.ID, fakeHash("d4"), "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE nodes SET current_version_id =
		'44444444-4444-4444-8444-444444444444', revision = 2 WHERE id = ?`, live.ID)
	require.NoError(t, err)

	// Nothing unreachable yet.
	un, err := s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.Empty(t, un)

	// Trashed-but-not-emptied stays reachable.
	_, _, err = s.Trash(ctx, trashed.ID, -1)
	require.NoError(t, err)
	un, err = s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	assert.Empty(t, un)

	// Hard-deleting a node makes its blob unreachable.
	_, _, err = s.Trash(ctx, gone.ID, -1)
	require.NoError(t, err)
	// Only 'gone' and 'trashed' are in the trash; empty just 'gone' by
	// deleting it directly (EmptyTrash cutoffs are time-based).
	_, err = s.db.Exec(`DELETE FROM nodes WHERE id = ?`, gone.ID)
	require.NoError(t, err)

	un, err = s.UnreachableBlobs(ctx)
	require.NoError(t, err)
	require.Len(t, un, 1)
	assert.Equal(t, fakeHash("c3"), un[0].Hash)
}

func TestDeleteBlobRows(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.db.Exec(`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
		fakeHash("a1"), "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	_, err = s.db.Exec(
		`INSERT INTO extracted_text (blob_hash, extractor, extractor_version, status, attempts, text, extracted_at)
		 VALUES (?, 'plain-text', 1, 'ok', 1, 'searchable', ?)`,
		fakeHash("a1"), "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO content_fts(rowid,blob_hash,extractor,text)
		SELECT rowid,blob_hash,extractor,text FROM extracted_text WHERE blob_hash=?`, fakeHash("a1"))
	require.NoError(t, err)

	require.NoError(t, s.DeleteBlobRows(ctx, []string{fakeHash("a1")}))

	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&n))
	assert.Equal(t, 0, n)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM extracted_text`).Scan(&n))
	assert.Equal(t, 0, n)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts`).Scan(&n))
	assert.Equal(t, 0, n)
}

func TestAllBlobs(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	_, err := s.CreateFile(ctx, s.RootID(), "b.txt", fakeHash("b2"), 2, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "a.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	blobs, err := s.AllBlobs(ctx)
	require.NoError(t, err)
	require.Len(t, blobs, 2)
	assert.Equal(t, fakeHash("a1"), blobs[0].Hash) // hash-ordered
	assert.Equal(t, int64(1), blobs[0].Size)
}
