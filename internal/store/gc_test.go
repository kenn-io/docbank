package store

import (
	"fmt"
	"slices"
	"strings"
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
			var after *string
			for {
				page, err := s.UnreachableBlobsPageFrom(ctx, after, 3)
				require.NoError(t, err)
				require.LessOrEqual(t, page.Examined, 3)
				for _, candidate := range page.Items {
					got = append(got, candidate.Hash)
				}
				if !page.More {
					break
				}
				require.Positive(t, page.Examined)
				after = &page.HighWater
			}
			assert.Equal(t, want, got)
		})
	}
}

func TestBlobInventoryResumeQueriesUseIndexedHashRange(t *testing.T) {
	s := newTestStore(t)
	after := fmt.Sprintf("%064x", 900)
	tests := []struct {
		name        string
		query       func(*string, int) (string, []any)
		index       string
		rangeClause string
	}{
		{name: "verify inventory", query: blobHashesPageQuery,
			index: "sqlite_autoindex_blobs_1", rangeClause: "(hash>?)"},
		{name: "unreachable inventory", query: unreachableBlobScanQuery,
			index: "sqlite_autoindex_blobs_1", rangeClause: "(hash>?)"},
		{name: "pack mapping inventory", query: unreferencedMappingScanQuery,
			index: "sqlite_autoindex_blob_pack_index_1", rangeClause: "(blob_hash>?)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query, args := test.query(&after, 3)
			rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
			require.NoError(t, err)
			defer func() { require.NoError(t, rows.Close()) }()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				details = append(details, detail)
			}
			require.NoError(t, rows.Err())
			plan := strings.Join(details, "\n")
			assert.Contains(t, plan, "SEARCH")
			assert.Contains(t, plan, test.index)
			assert.Contains(t, plan, test.rangeClause)
		})
	}
}

func TestBlobHashesPageNearEndUsesResumeKey(t *testing.T) {
	s := newTestStore(t)
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for i := range 5000 {
		_, err = tx.ExecContext(t.Context(),
			`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
			fmt.Sprintf("%064x", i), "2026-01-01T00:00:00.000000000Z")
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	after := fmt.Sprintf("%064x", 4995)
	hashes, more, err := s.BlobHashesPageFrom(t.Context(), &after, 3)
	require.NoError(t, err)
	assert.True(t, more)
	assert.Equal(t, []string{
		fmt.Sprintf("%064x", 4996),
		fmt.Sprintf("%064x", 4997),
		fmt.Sprintf("%064x", 4998),
	}, hashes)
}

func TestUnreachableBlobPageDistinguishesEmptyStoredKeyFromStart(t *testing.T) {
	s := newTestStore(t)
	later := "1000000000000000000000000000000000000000000000000000000000000000"
	for _, hash := range []string{"", later} {
		_, err := s.db.ExecContext(t.Context(),
			`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
			hash, "2026-01-01T00:00:00.000000000Z")
		require.NoError(t, err)
	}

	first, err := s.UnreachableBlobsPageFrom(t.Context(), nil, 1)
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	assert.Empty(t, first.Items[0].Hash)
	assert.Empty(t, first.HighWater)
	require.True(t, first.More)

	after := first.HighWater
	second, err := s.UnreachableBlobsPageFrom(t.Context(), &after, 1)
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	assert.Equal(t, later, second.Items[0].Hash)
	assert.False(t, second.More)
}

func TestUnreachableBlobScanBoundsExaminedLiveRun(t *testing.T) {
	for _, liveCount := range []int{8, 800} {
		t.Run(fmt.Sprintf("live=%d", liveCount), func(t *testing.T) {
			s := newTestStore(t)
			for i := 1; i <= liveCount; i++ {
				_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("live-%03d", i),
					fmt.Sprintf("%064x", i), 1, "application/octet-stream")
				require.NoError(t, err)
			}
			unreachable := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			_, err := s.db.ExecContext(t.Context(),
				`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
				unreachable, "2026-01-01T00:00:00.000000000Z")
			require.NoError(t, err)

			page, err := s.UnreachableBlobsPageFrom(t.Context(), nil, 4)
			require.NoError(t, err)
			assert.Empty(t, page.Items)
			assert.Equal(t, 4, page.Examined)
			assert.Equal(t, fmt.Sprintf("%064x", 4), page.HighWater)
			require.True(t, page.More)

			var got []string
			totalExamined := page.Examined
			for page.More {
				after := page.HighWater
				page, err = s.UnreachableBlobsPageFrom(t.Context(), &after, 4)
				require.NoError(t, err)
				require.LessOrEqual(t, page.Examined, 4)
				totalExamined += page.Examined
				for _, candidate := range page.Items {
					got = append(got, candidate.Hash)
				}
			}
			assert.Equal(t, []string{unreachable}, got)
			assert.Equal(t, liveCount+1, totalExamined)
		})
	}
}

func TestUnreferencedMappingScanBoundsExaminedLiveRun(t *testing.T) {
	for _, liveCount := range []int{8, 800} {
		t.Run(fmt.Sprintf("live=%d", liveCount), func(t *testing.T) {
			s := newTestStore(t)
			packID := pack.NewPackID()
			_, err := s.db.ExecContext(t.Context(), `
				INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at)
				VALUES (?, ?, ?, ?)`, packID, liveCount+1, (liveCount+1)*5,
				"2026-01-01T00:00:00.000000000Z")
			require.NoError(t, err)
			tx, err := s.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			for i := 1; i <= liveCount; i++ {
				hash := fmt.Sprintf("%064x", i)
				_, err = tx.ExecContext(t.Context(),
					`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
					hash, "2026-01-01T00:00:00.000000000Z")
				require.NoError(t, err)
				_, err = tx.ExecContext(t.Context(), `
					INSERT INTO blob_pack_index
						(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
					VALUES (?, ?, ?, 5, 1, 0, 0)`, hash, packID,
					pack.MinEntryOffset+int64(i-1)*32)
				require.NoError(t, err)
			}
			dangling := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			_, err = tx.ExecContext(t.Context(), `
				INSERT INTO blob_pack_index
					(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
				VALUES (?, ?, ?, 5, 1, 0, 0)`, dangling, packID,
				pack.MinEntryOffset+int64(liveCount)*32)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())

			page, err := s.UnreferencedPackMappingsPage(t.Context(), nil, 4)
			require.NoError(t, err)
			assert.Empty(t, page.Items)
			assert.Equal(t, 4, page.Examined)
			assert.Equal(t, fmt.Sprintf("%064x", 4), page.HighWater)
			require.True(t, page.More)

			var got []string
			totalExamined := page.Examined
			for page.More {
				after := page.HighWater
				page, err = s.UnreferencedPackMappingsPage(t.Context(), &after, 4)
				require.NoError(t, err)
				require.LessOrEqual(t, page.Examined, 4)
				totalExamined += page.Examined
				got = append(got, page.Items...)
			}
			assert.Equal(t, []string{dangling}, got)
			assert.Equal(t, liveCount+1, totalExamined)
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

func TestUnreferencedPackMappingsPageIsCanonicalAndBounded(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	packID := pack.NewPackID()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at)
		VALUES (?, 4, 40, ?)`, packID, "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	for i, hash := range []string{
		fmt.Sprintf("%064x", 40), fmt.Sprintf("%064x", 10),
		fmt.Sprintf("%064x", 30), fmt.Sprintf("%064x", 20),
	} {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO blob_pack_index
				(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
			VALUES (?, ?, ?, 5, 1, 0, 0)`, hash, packID, pack.MinEntryOffset+int64(i)*32)
		require.NoError(t, err)
	}

	first, err := s.UnreferencedPackMappingsPage(ctx, nil, 2)
	require.NoError(t, err)
	assert.True(t, first.More)
	assert.Equal(t, []string{fmt.Sprintf("%064x", 10), fmt.Sprintf("%064x", 20)}, first.Items)

	removed, err := s.DeleteUnreferencedPackMappings(ctx, first.Items)
	require.NoError(t, err)
	assert.Equal(t, int64(2), removed)
	second, err := s.UnreferencedPackMappingsPage(ctx, &first.HighWater, 2)
	require.NoError(t, err)
	assert.False(t, second.More)
	assert.Equal(t, []string{fmt.Sprintf("%064x", 30), fmt.Sprintf("%064x", 40)}, second.Items)

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`, second.Items[0], nowRFC3339())
	require.NoError(t, err)
	removed, err = s.DeleteUnreferencedPackMappings(ctx, second.Items)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed, "a concurrently restored authority row protects its mapping")
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

func TestSparseRepackScanBoundsExaminedIneligiblePacks(t *testing.T) {
	for _, total := range []int{8, 800} {
		t.Run(fmt.Sprintf("packs=%d", total), func(t *testing.T) {
			s := newTestStore(t)
			for i := range total {
				hash := fmt.Sprintf("%064x", i+1)
				packID := pack.NewPackID()
				_, err := s.db.ExecContext(t.Context(),
					`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
					hash, "2026-01-01T00:00:00.000000000Z")
				require.NoError(t, err)
				_, err = s.db.ExecContext(t.Context(), `
					INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at)
					VALUES (?, 1, 5, ?)`, packID, "2026-01-01T00:00:00.000000000Z")
				require.NoError(t, err)
				_, err = s.db.ExecContext(t.Context(), `
					INSERT INTO blob_pack_index
						(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
					VALUES (?, ?, ?, 5, 1, 0, 0)`, hash, packID, pack.MinEntryOffset)
				require.NoError(t, err)
			}

			page, err := s.SparseRepackScanPage(t.Context(), "", "", 4,
				time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC), time.Nanosecond, 1)
			require.NoError(t, err)
			assert.Len(t, page.Items, 4)
			assert.True(t, page.More)
			for _, item := range page.Items {
				assert.False(t, item.Eligible)
			}
			continued, err := s.SparseRepackScanPage(t.Context(),
				page.Items[len(page.Items)-1].Hash, page.Items[len(page.Items)-1].Usage.PackID, 4,
				time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC), time.Nanosecond, 1)
			require.NoError(t, err)
			require.Len(t, continued.Items, 4)
			assert.NotEqual(t, page.Items[0].Usage.PackID, continued.Items[0].Usage.PackID)
			assert.Equal(t, total > 8, continued.More)
		})
	}
}

func TestSparseRepackScanIncludesExactlyHalfLiveEvenPack(t *testing.T) {
	s := newTestStore(t)
	hash := fmt.Sprintf("%064x", 1)
	packID := pack.NewPackID()
	_, err := s.db.ExecContext(t.Context(),
		`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
		hash, "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `
		INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at)
		VALUES (?, 2, 10, ?)`, packID, "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `
		INSERT INTO blob_pack_index
			(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		VALUES (?, ?, ?, 5, 1, 0, 0)`, hash, packID, pack.MinEntryOffset)
	require.NoError(t, err)

	page, err := s.SparseRepackScanPage(t.Context(), "", "", 1,
		time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC), time.Nanosecond, 1)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.True(t, page.Items[0].Eligible,
		"one live entry in a two-entry pack is exactly half live")
}

func TestSparseRepackScanKeyStaysStableWhenEarlierPackLosesLiveness(t *testing.T) {
	s := newTestStore(t)
	packIDs := []string{pack.NewPackID(), pack.NewPackID()}
	slices.Sort(packIDs)
	hashes := []string{
		"1000000000000000000000000000000000000000000000000000000000000000",
		"2000000000000000000000000000000000000000000000000000000000000000",
	}
	for i, packID := range packIDs {
		_, err := s.db.ExecContext(t.Context(),
			`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
			hashes[i], "2026-01-01T00:00:00.000000000Z")
		require.NoError(t, err)
		_, err = s.db.ExecContext(t.Context(), `
			INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at)
			VALUES (?, 3, 30, ?)`, packID, "2026-01-01T00:00:00.000000000Z")
		require.NoError(t, err)
		_, err = s.db.ExecContext(t.Context(), `
			INSERT INTO blob_pack_index
				(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
			VALUES (?, ?, ?, 5, 1, 0, 0)`, hashes[i], packID, pack.MinEntryOffset)
		require.NoError(t, err)
	}

	first, err := s.SparseRepackScanPage(t.Context(), "", "", 1,
		time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC), time.Nanosecond, 1)
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	require.True(t, first.More)
	assert.Equal(t, packIDs[0], first.Items[0].Usage.PackID)
	assert.Equal(t, hashes[0], first.Items[0].Hash)

	_, err = s.db.ExecContext(t.Context(), `DELETE FROM blobs WHERE hash = ?`, hashes[0])
	require.NoError(t, err)
	var retainedScanHash string
	require.NoError(t, s.db.QueryRowContext(t.Context(),
		`SELECT scan_hash FROM blob_packs WHERE pack_id = ?`, packIDs[0]).Scan(&retainedScanHash))
	assert.Equal(t, hashes[0], retainedScanHash)
	second, err := s.SparseRepackScanPage(t.Context(), first.Items[0].Hash,
		first.Items[0].Usage.PackID, 1,
		time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC), time.Nanosecond, 1)
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	assert.Equal(t, packIDs[1], second.Items[0].Usage.PackID,
		"the continuation key is immutable even when the earlier pack becomes dead")
}

func TestPackScanHashDoesNotChangeAfterPackCreation(t *testing.T) {
	s := newTestStore(t)
	packID := pack.NewPackID()
	originalHash := "8000000000000000000000000000000000000000000000000000000000000000"
	laterHash := "1000000000000000000000000000000000000000000000000000000000000000"
	for _, hash := range []string{originalHash, laterHash} {
		_, err := s.db.ExecContext(t.Context(),
			`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
			hash, "2026-01-01T00:00:00.000000000Z")
		require.NoError(t, err)
	}
	_, err := s.db.ExecContext(t.Context(), `
		INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at, scan_hash)
		VALUES (?, 2, 10, ?, ?)`, packID, "2026-01-01T00:00:00.000000000Z", originalHash)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `
		INSERT INTO blob_pack_index
			(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		VALUES (?, ?, ?, 5, 1, 0, 0)`, originalHash, packID, pack.MinEntryOffset)
	require.NoError(t, err)

	_, err = s.db.ExecContext(t.Context(), `
		INSERT INTO blob_pack_index
			(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		VALUES (?, ?, ?, 5, 1, 0, 0)`, laterHash, packID, pack.MinEntryOffset+32)
	require.NoError(t, err)

	var scanHash string
	require.NoError(t, s.db.QueryRowContext(t.Context(),
		`SELECT scan_hash FROM blob_packs WHERE pack_id = ?`, packID).Scan(&scanHash))
	assert.Equal(t, originalHash, scanHash,
		"an established pack scan key remains immutable when mappings change")
}

func TestRepackSelectionQueriesUseSummaryIndexes(t *testing.T) {
	s := newTestStore(t)
	tests := []struct {
		name      string
		query     string
		args      []any
		wantIndex string
	}{
		{name: "dead", query: deadPackUsagePageSQL, args: []any{2},
			wantIndex: "blob_packs_dead_scan"},
		{name: "sparse", query: sparseRepackScanPageSQL, args: []any{"", "", "", 2},
			wantIndex: "blob_packs_live_scan"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+test.query, test.args...)
			require.NoError(t, err)
			defer func() { require.NoError(t, rows.Close()) }()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				details = append(details, detail)
			}
			require.NoError(t, rows.Err())
			plan := strings.Join(details, "\n")
			assert.Contains(t, plan, test.wantIndex)
			assert.NotContains(t, plan, "blob_pack_index")
			assert.NotContains(t, plan, "USE TEMP B-TREE")
		})
	}
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
