package processing_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

func TestResolveTextCitationPhysicalRestore(t *testing.T) {
	t.Parallel()
	f := processing.NewTextCitationTestFixture(t)
	f.ReplaceSource(t)
	_, err := maintenance.GarbageCollect(t.Context(), f.Catalog, f.Blobs, maintenance.GCOptions{})
	require.NoError(t, err)
	_, err = maintenance.Pack(t.Context(), f.Catalog, f.Blobs, 1<<20)
	require.NoError(t, err)
	physical, err := f.Catalog.PhysicalContent(t.Context(), f.Citation.RenditionSHA256)
	require.NoError(t, err)
	require.Equal(t, "packed", physical.Kind)
	want, err := f.Service.ResolveTextCitation(t.Context(), f.Citation)
	require.NoError(t, err)
	require.Equal(t, "é界🙂", want.Text)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repository"))
	require.NoError(t, err)
	_, err = backupapp.Create(t.Context(), repo, "test", f.Catalog, f.Blobs, backup.CreateOptions{Jobs: 2})
	require.NoError(t, err)
	proof, err := backup.Verify(t.Context(), repo, backupapp.New("test"), backup.VerifyOptions{Jobs: 2})
	require.NoError(t, err)
	require.Empty(t, proof.Problems)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test", backup.RestoreOptions{TargetDir: target, Jobs: 2})
	require.NoError(t, err)
	catalog, err := store.OpenForRestore(filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	service, err := processing.NewService(processing.ServiceConfig{Catalog: catalog, Blobs: blobs,
		Gate: api.NewOperationGate(), SpoolDirectory: filepath.Join(t.TempDir(), "spool")})
	require.NoError(t, err)
	got, err := service.ResolveTextCitation(t.Context(), f.Citation)
	require.NoError(t, err)
	require.Equal(t, want, got)
}
