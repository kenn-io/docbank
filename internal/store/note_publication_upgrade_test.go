package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNotePublicationUpgradeV48KeepsLocalObservations(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "released.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0170SQL, 48)
			seedWatchRebind(t, legacy, "note.md", "note.md")
			require.NoError(t, legacy.ReconcileWatchSourcePresence(t.Context(), "notes",
				WatchPresenceScan{ListedDirs: []string{""}}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
			// The generic fixture builder shadows old layouts' missing feed. Here
			// exercise the exact v48 feed tables that actually shipped.
			_, err := db.ExecContext(t.Context(), `DROP TABLE temp.vault_changes`)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `INSERT INTO vault_change_head(singleton,epoch) VALUES(1,?)`, metadataIngestID)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `INSERT INTO vault_changes(seq,kind,node_id,node_revision) VALUES(42,'content',2,1)`)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			upgraded, err := Open(t.Context(), path, driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
			requireAbsence(t, upgraded, "note.md", "2026-01-02T03:04:05Z")
			var epoch string
			require.NoError(t, upgraded.db.QueryRowContext(t.Context(), `SELECT epoch FROM vault_change_head WHERE singleton=1`).Scan(&epoch))
			require.Equal(t, metadataIngestID, epoch)
			var seq int64
			require.NoError(t, upgraded.db.QueryRowContext(t.Context(), `SELECT MAX(seq) FROM vault_changes`).Scan(&seq))
			require.EqualValues(t, 42, seq)
			require.FileExists(t, path+".schema-v48.bak")
		})
	}
}
