package store

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMissLogRetentionAndMetadataExclusion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	for _, entry := range []MissLogRecord{
		{ID: "old", CreatedAt: now.AddDate(0, 0, -3), Receipt: `{"query":"sentinel old"}`},
		{ID: "first", CreatedAt: now.Add(-time.Hour), Receipt: `{"query":"sentinel first"}`},
		{ID: "second", CreatedAt: now, Receipt: `{"query":"sentinel second"}`},
	} {
		require.NoError(t, s.AddMissLogRecord(ctx, entry, now, 2, 2))
	}
	entries, err := s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "second", entries[0].ID)
	require.Equal(t, "first", entries[1].ID)
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &metadata))
	require.NotContains(t, metadata.String(), "sentinel")
	require.NoError(t, s.DeleteMissLogRecord(ctx, "first"))
	require.ErrorIs(t, s.DeleteMissLogRecord(ctx, "missing"), ErrNotFound)
	entries, err = s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, s.PurgeMissLogRecords(ctx))
	entries, err = s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestMissLogExcludedFromBackupRestoreAndBundleMetadata(t *testing.T) {
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
	rows, err := restored.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	var records []any
	require.NoError(t, exportBundleMetadata(t.Context(), s.db, func(record any) error { records = append(records, record); return nil }))
	encoded, err := json.Marshal(records)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private sentinel query")
}

func TestMissLogConcurrentRetentionSQLite(t *testing.T) {
	t.Parallel()
	checkMissLogConcurrentRetention(t, newTestStore(t))
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
	records, err := s.ListMissLogRecords(t.Context(), 100, 0)
	require.NoError(t, err)
	require.Len(t, records, 3)
	require.Equal(t, []string{"record-19", "record-18", "record-17"}, []string{records[0].ID, records[1].ID, records[2].ID})
}

func TestMissLogOpensSchema43WithoutLocalTable(t *testing.T) {
	t.Parallel()
	for _, test := range v090UpgradeDrivers() {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vault.db")
			st, err := Open(path, test.driver)
			require.NoError(t, err)
			node, err := st.Mkdir(t.Context(), st.RootID(), "synthetic-retained")
			require.NoError(t, err)
			_, err = st.db.Exec(`DROP TABLE miss_log_records`)
			require.NoError(t, err)
			require.NoError(t, st.Close())
			reopened, err := Open(path, test.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			retained, err := reopened.NodeByID(t.Context(), node.ID)
			require.NoError(t, err)
			require.Equal(t, "synthetic-retained", retained.Name)
			require.NoError(t, reopened.AddMissLogRecord(t.Context(), MissLogRecord{ID: "new", CreatedAt: time.Now(), Receipt: `{}`}, time.Now(), 90, 5000))
		})
	}
}
