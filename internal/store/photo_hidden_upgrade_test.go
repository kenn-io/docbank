package store

import (
	"bytes"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

//go:embed testdata/schema-v30-addition.sql
var schemaV30AdditionSQL string

func TestPhotoHiddenUpgradeSchema30(t *testing.T) {
	t.Parallel()
	for _, driver := range v090UpgradeDrivers() {
		t.Run(driver.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docbank.db")
			db, legacy := newReleasedFixtureStore(t, path, driver.driver, schemaV0151SQL+schemaV30AdditionSQL, 30)
			node, err := legacy.CreateFile(t.Context(), 1, "synthetic-photo.jpg", fakeHash("a1"), 4, "image/jpeg")
			require.NoError(t, err)
			scores := document.PhotoQualitySignals{Blur: 1, Framing: 0.5}
			require.NoError(t, legacy.PublishPhotoQualitySignals(t.Context(), qualityTarget(node), scores))
			snapshot, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			var before bytes.Buffer
			require.NoError(t, exportReleasedMetadataSnapshot(t.Context(), snapshot, &before, 30))
			require.NoError(t, snapshot.Rollback())
			require.NoError(t, db.Close())
			s, err := Open(path, driver.driver)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()
			var version int
			require.NoError(t, s.db.QueryRow(`SELECT schema_version FROM vault_metadata`).Scan(&version))
			require.Equal(t, 31, version)
			var after bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &after))
			require.Equal(t, before.String(), after.String())
			quality, err := photoQualityForVersions(t.Context(), s.db, []string{node.CurrentVersionID})
			require.NoError(t, err)
			require.Equal(t, scores, quality.signals[node.CurrentVersionID])
			asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			require.Nil(t, asset.HiddenAt)
			require.FileExists(t, path+".schema-v30.bak")
			require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic"))
			_, err = s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
			require.NoError(t, err)
		})
	}
}
