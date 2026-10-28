package store

import (
	"bytes"
	"path/filepath"
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
	entries, err := s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "second", entries[0].ID)
	require.Equal(t, "first", entries[1].ID)
	require.NoError(t, s.DeleteMissLogRecord(ctx, "first"))
	require.ErrorIs(t, s.DeleteMissLogRecord(ctx, "missing"), ErrNotFound)
	entries, err = s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, s.PurgeMissLogRecords(ctx))
	require.Empty(t, events, "misses leave Photos unchanged")
	entries, err = s.ListMissLogRecords(ctx, 10, 0)
	require.NoError(t, err)
	require.Empty(t, entries)
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
