package store

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMissLogCountRetentionAndPhotoExclusion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	events := watchPhotoChanges(t, s)
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	for _, entry := range []MissLogRecord{
		{ID: "old", CreatedAt: now.Add(-2 * time.Hour), Receipt: `{"query":"sentinel old"}`},
		{ID: "first", CreatedAt: now.Add(-time.Hour), Receipt: `{"query":"sentinel first"}`},
		{ID: "second", CreatedAt: now, Receipt: `{"query":"sentinel second"}`},
	} {
		require.NoError(t, s.AddMissLogRecord(ctx, entry, now, 2, 2))
	}
	entries, err := s.ListMissLogRecords(ctx, time.Time{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "second", entries[0].ID)
	require.Equal(t, "first", entries[1].ID)
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &metadata))
	require.NotContains(t, metadata.String(), "sentinel")
	require.NoError(t, s.DeleteMissLogRecord(ctx, "first"))
	require.ErrorIs(t, s.DeleteMissLogRecord(ctx, "missing"), ErrNotFound)
	entries, err = s.ListMissLogRecords(ctx, time.Time{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, s.PurgeMissLogRecords(ctx))
	require.Empty(t, events, "misses leave Photos unchanged")
	entries, err = s.ListMissLogRecords(ctx, time.Time{}, 10, 0)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestMissLogPrunesExpiredRecords(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"old", "middle", "new"} {
		require.NoError(t, s.AddMissLogRecord(ctx, MissLogRecord{ID: id, CreatedAt: now.AddDate(0, 0, i-3), Receipt: `{}`}, now, 30, 10))
	}
	require.NoError(t, s.PruneMissLogRecords(ctx, now, 2, 10))
	entries, err := s.ListMissLogRecords(ctx, time.Time{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, []string{"new", "middle"}, []string{entries[0].ID, entries[1].ID})
}

func TestMissLogRetentionPostgres(t *testing.T) {
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"old", "middle", "new"} {
		require.NoError(t, s.AddMissLogRecord(ctx, MissLogRecord{ID: id, CreatedAt: now.AddDate(0, 0, i-3), Receipt: `{}`}, now, 30, 2))
	}
	entries, err := s.ListMissLogRecords(ctx, time.Time{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "new", entries[0].ID)
	require.NoError(t, s.PruneMissLogRecords(ctx, now, 1, 2))
	entries, err = s.ListMissLogRecords(ctx, time.Time{}, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "new", entries[0].ID)
}

func TestMissLogExcludedFromBackup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	now := time.Now()
	require.NoError(t, s.AddMissLogRecord(t.Context(), MissLogRecord{ID: "private-local", CreatedAt: now, Receipt: `{"query":"private sentinel query"}`}, now, 90, 5000))
	snapshot, err := s.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, snapshot.ExportBackup(t.Context(), &exported))
	require.NoError(t, snapshot.Close())
	require.NotContains(t, exported.String(), "private sentinel query")
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	rows, err := restored.ListMissLogRecords(t.Context(), time.Time{}, 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestMissLogConcurrentRetentionSQLite(t *testing.T) {
	t.Parallel()
	checkMissLogConcurrentRetention(t, newTestStore(t))
}

func TestMissLogConcurrentRetentionPostgres(t *testing.T) {
	t.Parallel()
	s := newPostgresQueryStore(t)
	s.writeDB.SetMaxOpenConns(4)
	checkMissLogConcurrentRetention(t, s)
}

func checkMissLogConcurrentRetention(t *testing.T, s *Store) {
	t.Helper()
	now := time.Now().UTC()
	start := make(chan struct{})
	failures := make(chan error, 20)
	var workers sync.WaitGroup
	for index := range 20 {
		workers.Go(func() {
			<-start
			failures <- s.AddMissLogRecord(t.Context(), MissLogRecord{ID: fmt.Sprintf("record-%02d", index), CreatedAt: now.Add(time.Duration(index) * time.Nanosecond), Receipt: `{"query":"synthetic concurrent miss"}`}, now, 90, 3)
		})
	}
	close(start)
	workers.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	records, err := s.ListMissLogRecords(t.Context(), time.Time{}, 100, 0)
	require.NoError(t, err)
	require.Len(t, records, 3)
	require.Equal(t, []string{"record-19", "record-18", "record-17"}, []string{records[0].ID, records[1].ID, records[2].ID})
}

func TestMissLogOpensSchema43WithoutLocalTable(t *testing.T) {
	t.Parallel()
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vault.db")
			st, err := Open(t.Context(), path, test.driver)
			require.NoError(t, err)
			node, err := st.Mkdir(t.Context(), st.RootID(), "synthetic-retained")
			require.NoError(t, err)
			_, err = st.db.ExecContext(t.Context(), `DROP TABLE miss_log_records`)
			require.NoError(t, err)
			require.NoError(t, st.Close())
			reopened, err := Open(t.Context(), path, test.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			retained, err := reopened.NodeByID(t.Context(), node.ID)
			require.NoError(t, err)
			require.Equal(t, "synthetic-retained", retained.Name)
			require.NoError(t, reopened.AddMissLogRecord(t.Context(), MissLogRecord{ID: "new", CreatedAt: time.Now(), Receipt: `{}`}, time.Now(), 90, 5000))
		})
	}
}

func TestMissLogMutationsKeepEmbeddingDiscoverySettled(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"add", "delete", "purge", "prune"} {
		t.Run(operation, func(t *testing.T) {
			s := newTestStore(t)
			now := time.Now().UTC()
			require.NoError(t, s.AddMissLogRecord(t.Context(), MissLogRecord{ID: "old", CreatedAt: now.Add(-72 * time.Hour), Receipt: `{}`}, now, 90, 5000))
			revision := s.WriteRevision()
			switch operation {
			case "add":
				require.NoError(t, s.AddMissLogRecord(t.Context(), MissLogRecord{ID: "new", CreatedAt: now, Receipt: `{}`}, now, 90, 5000))
			case "delete":
				require.NoError(t, s.DeleteMissLogRecord(t.Context(), "old"))
			case "purge":
				require.NoError(t, s.PurgeMissLogRecords(t.Context()))
			case "prune":
				require.NoError(t, s.PruneMissLogRecords(t.Context(), now, 1, 5000))
			}
			require.Equal(t, revision, s.WriteRevision(), "private search history keeps document discovery idle")
		})
	}
}
