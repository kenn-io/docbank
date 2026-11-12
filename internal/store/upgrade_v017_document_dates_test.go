package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpgradeReleasedV017RebuildsDocumentDateKeys(t *testing.T) {
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0170SQL, 48)
			createReleasedFixtureFile(t, legacy, "released.txt", fakeHash("d1"), 8, "text/plain")
			require.NoError(t, db.Close())
			s, err := Open(ctx, path, driver.driver)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			require.ErrorIs(t, requireDocumentDateIndex(ctx, s.db), ErrDocumentDatesPending)
			driveDocumentEventWorker(t, s)
			require.NoError(t, requireDocumentDateIndex(ctx, s.db))
			var count int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM document_date_keys`).Scan(&count))
			require.Positive(t, count)
		})
	}
}
