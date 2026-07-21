package docbank

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"

	internalmaintenance "go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

func TestGarbageCollectBudgetResumesEveryCandidateExactlyOnce(t *testing.T) {
	for _, test := range maintenanceDrivers() {
		t.Run(test.name, func(t *testing.T) {
			vault := newMaintenanceVault(t, test.driver)
			created := putMaintenanceFiles(t, vault, 7)
			trashMaintenanceFiles(t, vault, created)

			var removed int
			var cursor string
			for {
				report, err := vault.GarbageCollect(t.Context(), GCOptions{Budget: WorkBudget{
					MaxObjects: 3, Cursor: cursor,
				}})
				require.NoError(t, err)
				require.LessOrEqual(t, report.CandidateBlobs, 3)
				assert.Equal(t, report.CandidateBlobs, report.RemovedBlobs)
				removed += report.RemovedBlobs
				if !report.More {
					assert.Empty(t, report.NextCursor)
					break
				}
				require.NotEmpty(t, report.NextCursor)
				cursor = report.NextCursor
			}
			assert.Equal(t, len(created), removed)
			for _, item := range created {
				recorded, err := vault.metadata.HasBlob(t.Context(), item.Node.BlobHash)
				require.NoError(t, err)
				assert.False(t, recorded)
			}
		})
	}
}

func TestGarbageCollectRemovesZeroByteLooseBlob(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	created, err := vault.Put(t.Context(), "/empty", strings.NewReader(""), PutOptions{})
	require.NoError(t, err)
	trashMaintenanceFiles(t, vault, []PutReceipt{created})
	blobPath := filepath.Join(vault.root.Name(), "blobs", created.Node.BlobHash[:2], created.Node.BlobHash)
	require.FileExists(t, blobPath)

	report, err := vault.GarbageCollect(t.Context(), GCOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.ReclaimedFiles)
	assert.NoFileExists(t, blobPath)
}

func TestGarbageCollectMissingLooseFileDoesNotCountPhysicalReclamation(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	created, err := vault.Put(t.Context(), "/missing", strings.NewReader("missing"), PutOptions{})
	require.NoError(t, err)
	trashMaintenanceFiles(t, vault, []PutReceipt{created})
	blobPath := filepath.Join(vault.root.Name(), "blobs", created.Node.BlobHash[:2], created.Node.BlobHash)
	require.NoError(t, os.Remove(blobPath))

	report, err := vault.GarbageCollect(t.Context(), GCOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.CandidateBlobs)
	assert.Equal(t, 1, report.RemovedBlobs)
	assert.Zero(t, report.ReclaimedFiles,
		"only a file actually unlinked by this call is physically reclaimed")
}

func TestGarbageCollectDryRunPreservesRowsAndLooseFiles(t *testing.T) {
	for _, test := range maintenanceDrivers() {
		t.Run(test.name, func(t *testing.T) {
			vault := newMaintenanceVault(t, test.driver)
			created := putMaintenanceFiles(t, vault, 2)
			trashMaintenanceFiles(t, vault, created)

			report, err := vault.GarbageCollect(t.Context(), GCOptions{
				Budget: WorkBudget{MaxObjects: 1}, DryRun: true,
			})
			require.NoError(t, err)
			assert.Equal(t, 1, report.CandidateBlobs)
			assert.Zero(t, report.RemovedBlobs)
			assert.Zero(t, report.ReclaimedFiles)
			assert.True(t, report.More)
			for _, item := range created {
				recorded, recordErr := vault.metadata.HasBlob(t.Context(), item.Node.BlobHash)
				require.NoError(t, recordErr)
				assert.True(t, recorded)
				assert.FileExists(t, filepath.Join(vault.root.Name(), "blobs",
					item.Node.BlobHash[:2], item.Node.BlobHash))
			}
		})
	}
}

func TestGarbageCollectDoesNotScaleWithSingleShardPhysicalOrphans(t *testing.T) {
	var reports []GCReport
	for _, count := range []int{3, 500} {
		vault := newMaintenanceVault(t, nil)
		shard := filepath.Join(vault.root.Name(), "blobs", "aa")
		require.NoError(t, os.MkdirAll(shard, 0o700))
		for i := range count {
			hash := fmt.Sprintf("aa%062x", i)
			require.NoError(t, os.WriteFile(filepath.Join(shard, hash), []byte("orphan"), 0o600))
		}

		report, err := vault.GarbageCollect(t.Context(), GCOptions{Budget: WorkBudget{MaxObjects: 1}})
		require.NoError(t, err)
		reports = append(reports, report)
		assert.FileExists(t, filepath.Join(shard, fmt.Sprintf("aa%062x", count-1)))
	}
	require.Len(t, reports, 2)
	assert.Equal(t, reports[0], reports[1])
	assert.Zero(t, reports[1].UntrackedFiles)
	assert.False(t, reports[1].More)
}

func TestVerifyBudgetResumesEveryCandidateExactlyOnce(t *testing.T) {
	for _, test := range maintenanceDrivers() {
		t.Run(test.name, func(t *testing.T) {
			vault := newMaintenanceVault(t, test.driver)
			created := putMaintenanceFiles(t, vault, 7)
			want := make([]string, 0, len(created))
			for _, item := range created {
				want = append(want, item.Node.BlobHash)
				path := filepath.Join(vault.root.Name(), "blobs", item.Node.BlobHash[:2], item.Node.BlobHash)
				require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", int(item.Node.Size))), 0o600))
			}
			sort.Strings(want)

			var got []string
			var cursor string
			for {
				report, err := vault.Verify(t.Context(), VerifyOptions{Budget: WorkBudget{
					MaxObjects: 3, Cursor: cursor,
				}})
				require.NoError(t, err)
				require.LessOrEqual(t, report.OK+len(report.Problems), 3)
				for _, problem := range report.Problems {
					got = append(got, problem.Hash)
					assert.Equal(t, "corrupt", problem.Problem)
				}
				if !report.More {
					assert.Empty(t, report.NextCursor)
					break
				}
				require.NotEmpty(t, report.NextCursor)
				cursor = report.NextCursor
			}
			assert.Equal(t, want, got)
		})
	}
}

func TestVerifyCursorResumesPastMalformedStoredHash(t *testing.T) {
	for _, test := range maintenanceDrivers() {
		t.Run(test.name, func(t *testing.T) {
			vault := newMaintenanceVault(t, test.driver)
			hashes := []string{
				"0000000000000000000000000000000000000000000000000000000000000001",
				"1-malformed-boundary",
				"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			}
			db, err := vault.metadata.SQLiteDriver().Open(filepath.Join(vault.root.Name(), "docbank.db"),
				docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			for _, hash := range hashes {
				_, err = db.ExecContext(t.Context(),
					`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
					hash, "2026-07-21T00:00:00.000000000Z")
				require.NoError(t, err)
			}
			require.NoError(t, db.Close())

			first, err := vault.Verify(t.Context(), VerifyOptions{Budget: WorkBudget{MaxObjects: 2}})
			require.NoError(t, err)
			require.True(t, first.More)
			require.NotEmpty(t, first.NextCursor)
			assert.Equal(t, []VerifyProblem{
				{Hash: hashes[0], Problem: "missing"},
				{Hash: hashes[1], Problem: "unreadable"},
			}, first.Problems)

			second, err := vault.Verify(t.Context(), VerifyOptions{Budget: WorkBudget{
				MaxObjects: 2, Cursor: first.NextCursor,
			}})
			require.NoError(t, err)
			assert.False(t, second.More)
			assert.Equal(t, []VerifyProblem{{Hash: hashes[2], Problem: "missing"}}, second.Problems)
		})
	}
}

func TestVerifyCursorDistinguishesEmptyStoredKeyFromStart(t *testing.T) {
	for _, test := range maintenanceDrivers() {
		t.Run(test.name, func(t *testing.T) {
			vault := newMaintenanceVault(t, test.driver)
			db, err := vault.metadata.SQLiteDriver().Open(filepath.Join(vault.root.Name(), "docbank.db"),
				docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
			require.NoError(t, err)
			for _, hash := range []string{"", strings.Repeat("f", 64)} {
				_, err = db.ExecContext(t.Context(),
					`INSERT INTO blobs (hash, size, created_at) VALUES (?, 1, ?)`,
					hash, "2026-07-21T00:00:00.000000000Z")
				require.NoError(t, err)
			}
			require.NoError(t, db.Close())

			first, err := vault.Verify(t.Context(), VerifyOptions{Budget: WorkBudget{MaxObjects: 1}})
			require.NoError(t, err)
			require.True(t, first.More)
			assert.Equal(t, []VerifyProblem{{Hash: "", Problem: "unreadable"}}, first.Problems)
			second, err := vault.Verify(t.Context(), VerifyOptions{Budget: WorkBudget{
				MaxObjects: 1, Cursor: first.NextCursor,
			}})
			require.NoError(t, err)
			assert.False(t, second.More)
			assert.Equal(t, strings.Repeat("f", 64), second.Problems[0].Hash)
		})
	}
}

func TestVerifyDoesNotScaleWithUnrelatedMetadata(t *testing.T) {
	baseline := newMaintenanceVault(t, nil)
	_, err := baseline.Put(t.Context(), "/baseline", strings.NewReader("shared"), PutOptions{})
	require.NoError(t, err)
	baselineReport, err := baseline.Verify(t.Context(), VerifyOptions{Budget: WorkBudget{MaxObjects: 1}})
	require.NoError(t, err)

	large := newMaintenanceVault(t, nil)
	var hash string
	for i := range 500 {
		created, putErr := large.Put(t.Context(), fmt.Sprintf("/large-%03d", i),
			strings.NewReader("shared"), PutOptions{})
		require.NoError(t, putErr)
		hash = created.Node.BlobHash
	}
	db, err := large.metadata.SQLiteDriver().Open(filepath.Join(large.root.Name(), "docbank.db"),
		docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE blobs SET size='malformed' WHERE hash=?`, hash)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	largeReport, err := large.Verify(t.Context(), VerifyOptions{Budget: WorkBudget{MaxObjects: 1}})
	require.NoError(t, err)
	assert.Equal(t, baselineReport, largeReport,
		"bounded verification must not export or validate the unrelated metadata catalog")
}

func TestMaintenanceDefaultObjectAndSoftByteBudgetsAreFinite(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	putMaintenanceFiles(t, vault, DefaultMaintenanceMaxObjects+1)

	defaulted, err := vault.Verify(t.Context(), VerifyOptions{})
	require.NoError(t, err)
	assert.Equal(t, DefaultMaintenanceMaxObjects, defaulted.OK)
	assert.True(t, defaulted.More)
	require.NotEmpty(t, defaulted.NextCursor)

	byteBounded, err := vault.Verify(t.Context(), VerifyOptions{Budget: WorkBudget{MaxBytes: 1}})
	require.NoError(t, err)
	assert.Equal(t, 1, byteBounded.OK,
		"the soft byte budget admits one complete object before stopping")
	assert.True(t, byteBounded.More)
}

func TestMaintenanceCursorIsOpaqueAndOperationBound(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	created := putMaintenanceFiles(t, vault, 2)
	trashMaintenanceFiles(t, vault, created)

	preview, err := vault.GarbageCollect(t.Context(), GCOptions{
		Budget: WorkBudget{MaxObjects: 1}, DryRun: true,
	})
	require.NoError(t, err)
	require.True(t, preview.More)
	require.NotEmpty(t, preview.NextCursor)
	assert.NotContains(t, preview.NextCursor, created[0].Node.BlobHash)

	_, err = vault.Verify(t.Context(), VerifyOptions{Budget: WorkBudget{
		MaxObjects: 1, Cursor: preview.NextCursor,
	}})
	require.ErrorIs(t, err, ErrInvalidMaintenanceCursor)
	_, err = vault.GarbageCollect(t.Context(), GCOptions{Budget: WorkBudget{
		MaxObjects: 1, Cursor: "not-a-cursor",
	}})
	require.ErrorIs(t, err, ErrInvalidMaintenanceCursor)
	invalidSparseStart := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":1,"op":"repack","phase":"sparse","hash":"","set":true}`))
	_, err = vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{Cursor: invalidSparseStart}})
	require.ErrorIs(t, err, ErrInvalidMaintenanceCursor)
}

func TestPackReportsIndexedLooseBacklog(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	putMaintenanceFiles(t, vault, 2)

	first, err := vault.Pack(t.Context(), PackOptions{MaxBytes: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, first.BlobsPacked)
	assert.True(t, first.BudgetExhausted)
	assert.True(t, first.More)

	second, err := vault.Pack(t.Context(), PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, second.BlobsPacked)
	assert.False(t, second.More)
}

func TestRepackPreservesRawByteBudgetResumeAndCancellation(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	var dead []PutReceipt
	for batch := range 2 {
		items := putMaintenanceFilesAt(t, vault, batch*3, 3)
		dead = append(dead, items[:2]...)
		packed, err := vault.Pack(t.Context(), PackOptions{})
		require.NoError(t, err)
		require.Equal(t, 3, packed.BlobsPacked)
	}
	trashMaintenanceFiles(t, vault, dead)
	_, err := vault.GarbageCollect(t.Context(), GCOptions{})
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = vault.Repack(canceled, RepackOptions{Budget: WorkBudget{MaxBytes: 1},
		MinAge: time.Nanosecond, MinDeadBytes: 1})
	require.ErrorIs(t, err, context.Canceled)

	first, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{
		MaxObjects: 1, MaxBytes: 1,
	}, MinAge: time.Nanosecond, MinDeadBytes: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, first.PacksRewritten)
	assert.Positive(t, first.BytesRepacked)
	assert.True(t, first.More)
	require.NotEmpty(t, first.NextCursor)

	second, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{
		MaxObjects: 1, MaxBytes: 1, Cursor: first.NextCursor,
	}, MinAge: time.Nanosecond, MinDeadBytes: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, second.PacksRewritten)
	assert.False(t, second.More)
	assert.Empty(t, second.NextCursor)
}

func TestRepackBoundsDeadPackRetirementWithoutCursor(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	createDeadMaintenancePacks(t, vault, 3)

	var removed int
	for call := range 3 {
		report, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{MaxObjects: 1}})
		require.NoError(t, err)
		assert.Equal(t, 1, report.PacksRemoved)
		if report.More {
			require.NotEmpty(t, report.NextCursor)
		}
		assert.Equal(t, call < 2, report.More)
		removed += report.PacksRemoved
	}
	assert.Equal(t, 3, removed)
}

func TestRepackDeadRetirementFailureReturnsErrorAndDoesNotStarveLaterPack(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	createDeadMaintenancePacks(t, vault, 2)
	records, err := store.NewPackCatalog(vault.metadata).ListPackRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 2)
	sort.Slice(records, func(i, j int) bool { return records[i].PackID < records[j].PackID })
	blockedPath := filepath.Join(vault.root.Name(), "blobs", "packs", records[0].PackID[:2],
		records[0].PackID+packstore.PackExt)
	require.NoError(t, os.Remove(blockedPath))
	require.NoError(t, os.Mkdir(blockedPath, 0o700),
		"a directory at the pack path makes file retirement fail on every supported platform")
	require.NoError(t, os.WriteFile(filepath.Join(blockedPath, "keep"), []byte("occupied"), 0o600))

	report, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{MaxObjects: 2}})
	require.Error(t, err)
	assert.Equal(t, 1, report.PacksRemoved, "the later dead pack still retires")
	assert.True(t, report.More, "the failed physical retirement remains retryable")
	records, err = store.NewPackCatalog(vault.metadata).ListPackRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, filepath.Base(blockedPath), records[0].PackID+packstore.PackExt)

	require.NoError(t, os.Remove(filepath.Join(blockedPath, "keep")))
	require.NoError(t, os.Remove(blockedPath))

	retry, retryErr := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{MaxObjects: 2}})
	require.NoError(t, retryErr)
	assert.Equal(t, 1, retry.PacksRemoved)
	assert.False(t, retry.More)
	records, err = store.NewPackCatalog(vault.metadata).ListPackRecords(t.Context())
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestRepackDeadPhaseCursorDefersNewMappingsUntilFreshCycle(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	createDeadMaintenancePacks(t, vault, 2)
	stable := putMaintenanceFilesAt(t, vault, 950, 1)
	packed, err := vault.Pack(t.Context(), PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.BlobsPacked)
	records, err := store.NewPackCatalog(vault.metadata).ListPackRecords(t.Context())
	require.NoError(t, err)
	var stablePackID string
	for _, record := range records {
		entries, entryErr := store.NewPackCatalog(vault.metadata).ListPackEntries(t.Context(), record.PackID)
		require.NoError(t, entryErr)
		if len(entries) == 1 && entries[0].Hash.String() == stable[0].Node.BlobHash {
			stablePackID = record.PackID
		}
	}
	require.NotEmpty(t, stablePackID)

	first, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{MaxObjects: 1}})
	require.NoError(t, err)
	require.True(t, first.More)
	require.NotEmpty(t, first.NextCursor)

	insertDanglingMaintenanceMapping(t, vault,
		"0000000000000000000000000000000000000000000000000000000000000001", stablePackID)
	second, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{
		MaxObjects: 10, Cursor: first.NextCursor,
	}})
	require.NoError(t, err)
	assert.Zero(t, second.MappingsPruned,
		"dead-phase continuation must not restart the completed mapping phase")

	fresh, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{MaxObjects: 10}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), fresh.MappingsPruned)
}

func TestRepackAutomaticModeContinuesPastCorruptSparseSource(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	var dead []PutReceipt
	for batch := range 2 {
		items := putMaintenanceFilesAt(t, vault, 200+batch*3, 3)
		dead = append(dead, items[:2]...)
		packed, err := vault.Pack(t.Context(), PackOptions{})
		require.NoError(t, err)
		require.Equal(t, 3, packed.BlobsPacked)
	}
	trashMaintenanceFiles(t, vault, dead)
	_, err := vault.GarbageCollect(t.Context(), GCOptions{})
	require.NoError(t, err)
	candidates, _, err := vault.metadata.SparseRepackPage(t.Context(), "", 2,
		time.Now().UTC(), time.Nanosecond, 1)
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	corruptPath := filepath.Join(vault.root.Name(), "blobs", "packs",
		candidates[0].Usage.PackID[:2], candidates[0].Usage.PackID+packstore.PackExt)
	require.NoError(t, os.Truncate(corruptPath, 1))

	report, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{
		MaxObjects: 2, MaxBytes: 1,
	}, MinAge: time.Nanosecond, MinDeadBytes: 1})
	require.Error(t, err)
	assert.Equal(t, 1, report.PacksRewritten,
		"automatic repack preserves Kit's source-independent failure behavior")
	assert.True(t, report.More)
	require.NotEmpty(t, report.NextCursor,
		"the explicit dead-phase cursor retries the failed lowest canonical hash")
}

func TestRepackPrunesMappingsWithinBudgetWithoutPhysicalPack(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	db, err := vault.metadata.SQLiteDriver().Open(filepath.Join(vault.root.Name(), "docbank.db"),
		docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	packID := pack.NewPackID()
	hash := fmt.Sprintf("%064x", 99)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at)
		VALUES (?, 1, 20, ?)`, packID, "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO blob_pack_index
			(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		VALUES (?, ?, ?, 1, 1, 0, 0)`, hash, packID, pack.MinEntryOffset)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	report, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{MaxObjects: 1}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), report.MappingsPruned)
	assert.Zero(t, report.PacksSelected)
	assert.True(t, report.More)
	require.NotEmpty(t, report.NextCursor)
	indexed, err := store.NewPackCatalog(vault.metadata).ListIndexed(t.Context())
	require.NoError(t, err)
	assert.Empty(t, indexed)
}

func TestRepackPrunesMappingForPresentPackBeforePhysicalWork(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	created := putMaintenanceFiles(t, vault, 1)
	packed, err := vault.Pack(t.Context(), PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, packed.BlobsPacked)
	records, err := store.NewPackCatalog(vault.metadata).ListPackRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	packPath := filepath.Join(vault.root.Name(), "blobs", "packs", records[0].PackID[:2],
		records[0].PackID+packstore.PackExt)
	require.FileExists(t, packPath)
	trashMaintenanceFiles(t, vault, created)
	db, err := vault.metadata.SQLiteDriver().Open(filepath.Join(vault.root.Name(), "docbank.db"),
		docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `DELETE FROM blobs WHERE hash=?`, created[0].Node.BlobHash)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	report, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{MaxObjects: 1}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), report.MappingsPruned)
	assert.Zero(t, report.PacksSelected)
	assert.True(t, report.More)
	assert.FileExists(t, packPath, "mapping reconciliation consumes the finite page before retirement")
}

func TestRepackStopsAfterNonSourceFailureBeforeLaterPackMutation(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	createSparseMaintenancePacks(t, vault, 2)
	candidates, _, err := vault.metadata.SparseRepackPage(t.Context(), "", 2,
		time.Now().UTC(), time.Nanosecond, 1)
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	sentinel := errors.New("catalog unavailable")
	catalog := &repackFaultCatalog{Catalog: store.NewPackCatalog(vault.metadata),
		failListPack: candidates[0].Usage.PackID, laterPack: candidates[1].Usage.PackID,
		listErr: sentinel}

	report, err := internalmaintenance.Repack(t.Context(), vault.metadata, vault.blobs,
		internalmaintenance.RepackOptions{Budget: internalmaintenance.Budget{
			MaxObjects: 2, MaxBytes: 1,
		}, MinAge: time.Nanosecond, MinDeadBytes: 1, Catalog: catalog})
	require.ErrorIs(t, err, sentinel)
	assert.Zero(t, report.PacksRewritten)
	assert.False(t, catalog.laterRead, "a catalog failure must stop before opening the later source")
}

func TestRepackProbesWorkAfterPostRewriteRetirementError(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	createSparseMaintenancePacks(t, vault, 2)
	candidates, _, err := vault.metadata.SparseRepackPage(t.Context(), "", 2,
		time.Now().UTC(), time.Nanosecond, 1)
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	sentinel := errors.New("retirement acknowledgement lost")
	catalog := &repackFaultCatalog{Catalog: store.NewPackCatalog(vault.metadata),
		retirePack: candidates[1].Usage.PackID, retireThenErr: sentinel}

	report, err := internalmaintenance.Repack(t.Context(), vault.metadata, vault.blobs,
		internalmaintenance.RepackOptions{Budget: internalmaintenance.Budget{MaxObjects: 2},
			MinAge: time.Nanosecond, MinDeadBytes: 1, Catalog: catalog})
	require.ErrorIs(t, err, sentinel)
	assert.Positive(t, report.BytesRepacked)
	assert.Equal(t, 2, report.PacksRewritten, "the first canonical candidate completed before the fault")
	assert.True(t, report.More, "the replacement summary still needs bounded selection scanning")
	require.NotEmpty(t, report.NextCursor)

	resumed, resumeErr := internalmaintenance.Repack(t.Context(), vault.metadata, vault.blobs,
		internalmaintenance.RepackOptions{Budget: internalmaintenance.Budget{
			MaxObjects: 2, Cursor: report.NextCursor,
		}, MinAge: time.Nanosecond, MinDeadBytes: 1})
	require.NoError(t, resumeErr)
	assert.False(t, resumed.More)
}

func TestRepackIneligibleOnlyScanAdvancesCursor(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	for i := range 9 { // MaxObjects 1 examines eight summaries per deterministic scan window.
		putMaintenanceFilesAt(t, vault, 1200+i, 1)
		packed, err := vault.Pack(t.Context(), PackOptions{})
		require.NoError(t, err)
		require.Equal(t, 1, packed.BlobsPacked)
	}

	first, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{MaxObjects: 1},
		MinAge: time.Nanosecond, MinDeadBytes: 1})
	require.NoError(t, err)
	assert.Zero(t, first.PacksSelected)
	require.True(t, first.More)
	require.NotEmpty(t, first.NextCursor)

	second, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{
		MaxObjects: 1, Cursor: first.NextCursor,
	}, MinAge: time.Nanosecond, MinDeadBytes: 1})
	require.NoError(t, err)
	assert.Zero(t, second.PacksSelected)
	assert.False(t, second.More)
}

func TestRepackPreservesMappingHighWaterAcrossDeadPackPages(t *testing.T) {
	for _, test := range maintenanceDrivers() {
		t.Run(test.name, func(t *testing.T) {
			vault := newMaintenanceVault(t, test.driver)
			createDeadMaintenancePacks(t, vault, 2)
			createSparseMaintenancePacks(t, vault, 1)
			putMaintenanceFilesAt(t, vault, 900, 2)
			packed, err := vault.Pack(t.Context(), PackOptions{})
			require.NoError(t, err)
			require.Equal(t, 2, packed.BlobsPacked)

			dead, _, err := vault.metadata.DeadPackUsagePage(t.Context(), 10)
			require.NoError(t, err)
			require.Len(t, dead, 2)
			sparse, _, err := vault.metadata.SparseRepackPage(t.Context(), "", 10,
				time.Now().UTC(), time.Nanosecond, 1)
			require.NoError(t, err)
			require.Len(t, sparse, 1)
			records, err := store.NewPackCatalog(vault.metadata).ListPackRecords(t.Context())
			require.NoError(t, err)
			excluded := map[string]bool{sparse[0].Usage.PackID: true}
			for _, usage := range dead {
				excluded[usage.PackID] = true
			}
			var stablePackID string
			for _, record := range records {
				if !excluded[record.PackID] {
					stablePackID = record.PackID
				}
			}
			require.NotEmpty(t, stablePackID)

			highHash := "fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe"
			insertDanglingMaintenanceMapping(t, vault, highHash, dead[0].PackID)
			first, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{MaxObjects: 2},
				MinAge: time.Nanosecond, MinDeadBytes: 1})
			require.NoError(t, err)
			assert.Equal(t, int64(1), first.MappingsPruned)
			assert.Equal(t, 1, first.PacksRemoved)
			assert.True(t, first.More)
			require.NotEmpty(t, first.NextCursor,
				"dead-pack paging must retain the completed mapping phase's high-water mark")

			lowHash := "0000000000000000000000000000000000000000000000000000000000000001"
			insertDanglingMaintenanceMapping(t, vault, lowHash, stablePackID)
			second, err := vault.Repack(t.Context(), RepackOptions{Budget: WorkBudget{
				MaxObjects: 10, Cursor: first.NextCursor,
			}, MinAge: time.Nanosecond, MinDeadBytes: 1})
			require.NoError(t, err)
			assert.Zero(t, second.MappingsPruned,
				"a newly inserted lower mapping waits for the next empty-cursor cycle")
			assert.False(t, second.More)
			indexed, err := store.NewPackCatalog(vault.metadata).ListIndexed(t.Context())
			require.NoError(t, err)
			var lowerMappingPresent bool
			for _, entry := range indexed {
				lowerMappingPresent = lowerMappingPresent || entry.Hash.String() == lowHash
			}
			assert.True(t, lowerMappingPresent)
		})
	}
}

func TestMaintenanceRejectsClosedVault(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	_, err = vault.GarbageCollect(t.Context(), GCOptions{})
	require.ErrorIs(t, err, ErrClosed)
	_, err = vault.Verify(t.Context(), VerifyOptions{})
	require.ErrorIs(t, err, ErrClosed)
	_, err = vault.Repack(t.Context(), RepackOptions{})
	require.ErrorIs(t, err, ErrClosed)
}

type maintenanceDriver struct {
	name   string
	driver docsqlite.Driver
}

func maintenanceDrivers() []maintenanceDriver {
	return []maintenanceDriver{{name: "build default"}, {name: "pure Go", driver: modernc.Driver{}}}
}

func newMaintenanceVault(t *testing.T, driver docsqlite.Driver) *Vault {
	t.Helper()
	vault, err := New(t.Context(), Config{Root: t.TempDir(), SQLite: driver})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	return vault
}

func putMaintenanceFiles(t *testing.T, vault *Vault, count int) []PutReceipt {
	t.Helper()
	return putMaintenanceFilesAt(t, vault, 0, count)
}

func putMaintenanceFilesAt(t *testing.T, vault *Vault, start, count int) []PutReceipt {
	t.Helper()
	result := make([]PutReceipt, 0, count)
	for i := range count {
		index := start + i
		created, err := vault.Put(t.Context(), fmt.Sprintf("/maintenance-%03d", index),
			strings.NewReader(fmt.Sprintf("maintenance-content-%03d", index)), PutOptions{})
		require.NoError(t, err)
		result = append(result, created)
	}
	return result
}

func trashMaintenanceFiles(t *testing.T, vault *Vault, files []PutReceipt) {
	t.Helper()
	for _, file := range files {
		_, err := vault.TrashPath(t.Context(), "/"+file.Node.Name, RevisionOptions{})
		require.NoError(t, err)
	}
	for {
		report, err := vault.EmptyTrash(t.Context(), TrashEmptyOptions{MaxRoots: 3})
		require.NoError(t, err)
		if !report.More {
			return
		}
	}
}

func createDeadMaintenancePacks(t *testing.T, vault *Vault, count int) {
	t.Helper()
	for i := range count {
		created := putMaintenanceFilesAt(t, vault, 100+i, 1)
		packed, err := vault.Pack(t.Context(), PackOptions{})
		require.NoError(t, err)
		require.Equal(t, 1, packed.BlobsPacked)
		trashMaintenanceFiles(t, vault, created)
		collected, err := vault.GarbageCollect(t.Context(), GCOptions{})
		require.NoError(t, err)
		require.Equal(t, 1, collected.RemovedBlobs)
	}
}

func createSparseMaintenancePacks(t *testing.T, vault *Vault, count int) {
	t.Helper()
	var dead []PutReceipt
	for batch := range count {
		items := putMaintenanceFilesAt(t, vault, 500+batch*3, 3)
		dead = append(dead, items[:2]...)
		packed, err := vault.Pack(t.Context(), PackOptions{})
		require.NoError(t, err)
		require.Equal(t, 3, packed.BlobsPacked)
	}
	trashMaintenanceFiles(t, vault, dead)
	_, err := vault.GarbageCollect(t.Context(), GCOptions{})
	require.NoError(t, err)
}

func insertDanglingMaintenanceMapping(t *testing.T, vault *Vault, hash, packID string) {
	t.Helper()
	db, err := vault.metadata.SQLiteDriver().Open(filepath.Join(vault.root.Name(), "docbank.db"),
		docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO blob_pack_index
			(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		VALUES (?, ?, ?, 1, 1, 0, 0)`, hash, packID, pack.MinEntryOffset)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

type repackFaultCatalog struct {
	packstore.Catalog

	failListPack  string
	laterPack     string
	listErr       error
	retirePack    string
	retireThenErr error
	laterRead     bool
}

func (c *repackFaultCatalog) ListLivePackEntries(
	ctx context.Context, packID string,
) ([]packstore.IndexEntry, error) {
	if packID == c.laterPack {
		c.laterRead = true
	}
	if packID == c.failListPack {
		return nil, c.listErr
	}
	entries, err := c.Catalog.ListLivePackEntries(ctx, packID)
	if err != nil {
		return nil, fmt.Errorf("listing live entries through fault catalog: %w", err)
	}
	return entries, nil
}

func (c *repackFaultCatalog) DeleteEmptyPackRecord(ctx context.Context, packID string) (bool, error) {
	deleted, err := c.Catalog.DeleteEmptyPackRecord(ctx, packID)
	if err != nil {
		return deleted, fmt.Errorf("deleting empty pack through fault catalog: %w", err)
	}
	if packID != c.retirePack {
		return deleted, nil
	}
	return deleted, c.retireThenErr
}

func TestMaintenanceNegativeBudgetsAreRejected(t *testing.T) {
	vault := newMaintenanceVault(t, nil)
	_, err := vault.Verify(t.Context(), VerifyOptions{Budget: WorkBudget{MaxObjects: -1}})
	require.Error(t, err)
	_, err = vault.GarbageCollect(t.Context(), GCOptions{Budget: WorkBudget{MaxBytes: -1}})
	require.Error(t, err)
	_, err = vault.Repack(t.Context(), RepackOptions{MinAge: -time.Second})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrClosed)
}
