//go:build cgo

package backupapp_test

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

type recordingDriver struct {
	inner docsqlite.Driver
	mu    sync.Mutex
	opens []docsqlite.AccessMode
}

func (d *recordingDriver) Name() string { return "recording-" + d.inner.Name() }

func (d *recordingDriver) Open(path string, opts docsqlite.OpenOptions) (*sql.DB, error) {
	d.mu.Lock()
	d.opens = append(d.opens, opts.Access)
	d.mu.Unlock()
	return d.inner.Open(path, opts)
}

func (d *recordingDriver) IsBusy(err error) bool { return d.inner.IsBusy(err) }

func (d *recordingDriver) IsUniqueViolation(err error) bool {
	return d.inner.IsUniqueViolation(err)
}

func (d *recordingDriver) accesses() []docsqlite.AccessMode {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]docsqlite.AccessMode(nil), d.opens...)
}

func TestRestoreUsesNonDefaultSQLiteDriverThroughout(t *testing.T) {
	require := require.New(t)
	fixture := newArchiveFixture(t)
	wantMetadata := exportMetadata(t, fixture.metadata)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	_, err = backupapp.Create(
		t.Context(), repo, "test-version", fixture.metadata, fixture.blobs, backup.CreateOptions{})
	require.NoError(err)

	target := filepath.Join(t.TempDir(), "restored")
	driver := &recordingDriver{inner: modernc.Driver{}}
	_, err = backupapp.RestoreWithDriver(
		t.Context(), repo, "test-version", driver,
		backup.RestoreOptions{TargetDir: target})
	require.NoError(err)
	accesses := driver.accesses()
	require.Contains(accesses, docsqlite.Create, "metadata import must use the selected driver")
	require.Contains(accesses, docsqlite.ReadOnlyImmutable, "restore proof must use the selected driver")
	require.Contains(accesses, docsqlite.ReadWriteExisting, "packed catalog must use the selected driver")

	restored, err := store.Open(filepath.Join(target, "docbank.db"), modernc.Driver{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(restored.Close()) })
	require.Equal(string(wantMetadata), string(exportMetadata(t, restored)))
}
