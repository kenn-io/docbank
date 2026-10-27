package store

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
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
	entries, err = s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, s.PurgeMissLogRecords(ctx))
	entries, err = s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestMissLogPrunesOnStartupAndDeletesBeforeDate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"old", "middle", "new"} {
		require.NoError(t, s.AddMissLogRecord(ctx, MissLogRecord{ID: id, CreatedAt: now.AddDate(0, 0, i-3), Receipt: `{}`}, now, 30, 10))
	}
	require.NoError(t, s.PruneMissLogRecords(ctx, now, 2, 10))
	entries, err := s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.NoError(t, s.DeleteMissLogRecordsBefore(ctx, now.AddDate(0, 0, -1)))
	entries, err = s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "new", entries[0].ID)
}

func TestMissLogRetentionPostgres(t *testing.T) {
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"old", "middle", "new"} {
		require.NoError(t, s.AddMissLogRecord(ctx, MissLogRecord{ID: id, CreatedAt: now.AddDate(0, 0, i-3), Receipt: `{}`}, now, 30, 2))
	}
	entries, err := s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "new", entries[0].ID)
	require.NoError(t, s.PruneMissLogRecords(ctx, now, 1, 2))
	entries, err = s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, s.DeleteMissLogRecordsBefore(ctx, now))
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
	records, err := s.ListMissLogRecords(t.Context(), 100, 0)
	require.NoError(t, err)
	require.Len(t, records, 3)
	require.Equal(t, []string{"record-19", "record-18", "record-17"}, []string{records[0].ID, records[1].ID, records[2].ID})
}
