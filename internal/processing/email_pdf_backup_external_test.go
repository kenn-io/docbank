package processing_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

func TestEmailPDFRetainedReceiptReuseAndPhysicalBackup(t *testing.T) {
	for _, released := range []bool{false, true} {
		name := "fresh"
		if released {
			name = "released-v0.9-upgrade"
		}
		t.Run(name, func(t *testing.T) {
			processing.RunEmailPDFRetentionTest(t, released, func(catalog *store.Store, blobs *blob.Store) (*store.Store, *blob.Store) {
				repository, err := backup.Init(filepath.Join(t.TempDir(), "repository"))
				require.NoError(t, err)
				_, err = backupapp.Create(t.Context(), repository, "email-pdf", catalog, blobs, backup.CreateOptions{Jobs: 2})
				require.NoError(t, err)
				proof, err := backup.Verify(t.Context(), repository, backupapp.New("email-pdf"), backup.VerifyOptions{Jobs: 2})
				require.NoError(t, err)
				require.Empty(t, proof.Problems)
				destination := filepath.Join(t.TempDir(), "restored")
				_, err = backupapp.Restore(t.Context(), repository, "email-pdf", backup.RestoreOptions{TargetDir: destination, Jobs: 2})
				require.NoError(t, err)
				restored, err := store.OpenForRestore(filepath.Join(destination, "docbank.db"), store.DefaultSQLiteDriver())
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, restored.Close()) })
				bs, err := blob.New(store.NewPackCatalog(restored), filepath.Join(destination, "blobs"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, bs.Close()) })
				return restored, bs
			})
		})
	}
}

func TestEmailPDFRealBackupFixture(t *testing.T) {
	processing.RunEmailPDFRealBackupFixture(t, func(path string, catalog *store.Store, blobs *blob.Store) {
		repository, err := backup.Init(path)
		require.NoError(t, err)
		_, err = backupapp.Create(t.Context(), repository, "email-pdf", catalog, blobs, backup.CreateOptions{Jobs: 2})
		require.NoError(t, err)
		verified, err := backup.Verify(t.Context(), repository, backupapp.New("email-pdf"), backup.VerifyOptions{Jobs: 2})
		require.NoError(t, err)
		require.Empty(t, verified.Problems)
	})
}
