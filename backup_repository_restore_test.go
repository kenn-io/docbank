package docbank_test

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank"
	"go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

func TestRepositoryRestoreLargeExtraAfterSourceLoss(t *testing.T) {
	r := require.New(t)
	size := int64(64<<20 + 1)
	if value := os.Getenv("DOCBANK_LARGE_OBJECT_TEST_BYTES"); value != "" {
		var err error
		size, err = strconv.ParseInt(value, 10, 64)
		r.NoError(err)
		r.Greater(size, int64(64<<20))
	}
	source := filepath.Join(t.TempDir(), "source")
	vault, err := docbank.New(t.Context(), docbank.Config{Root: source})
	r.NoError(err)
	t.Cleanup(func() { r.NoError(vault.Close()) })
	repositoryPath := filepath.Join(t.TempDir(), "repository")
	repository, err := docbank.InitBackupRepository(repositoryPath)
	r.NoError(err)
	extra, err := os.CreateTemp(t.TempDir(), "recovery-*")
	r.NoError(err)
	t.Cleanup(func() { _ = extra.Close() })
	r.NoError(extra.Truncate(size))
	_, err = extra.WriteAt([]byte("synthetic recovery header"), 0)
	r.NoError(err)
	_, err = extra.WriteAt([]byte("tail"), size-4)
	r.NoError(err)
	digest := sha256.New()
	n, err := io.Copy(digest, extra)
	r.NoError(err)
	r.Equal(size, n)
	wantHash := digest.Sum(nil)
	r.NoError(extra.Close())
	snapshot, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{
		ExtraFiles: []docbank.BackupExtraFile{{Path: extra.Name(), RecordAs: "application/recovery.bin"}},
	})
	r.NoError(err)
	r.Equal(6, snapshot.MinReaderVersion)
	r.NoError(vault.Close())
	r.NoError(os.RemoveAll(source))
	r.NoError(os.Remove(extra.Name()))
	moved := filepath.Join(t.TempDir(), "moved-repository")
	r.NoError(os.Rename(repositoryPath, moved))
	repository, err = docbank.OpenBackupRepository(moved)
	r.NoError(err)
	snapshots, err := repository.Snapshots()
	r.NoError(err)
	r.Equal([]docbank.BackupSnapshot{snapshot}, snapshots)
	_, err = repository.Prune(t.Context(), docbank.BackupPruneOptions{})
	r.NoError(err)
	verified, err := repository.Verify(t.Context(), docbank.BackupVerifyOptions{SnapshotID: snapshot.ID})
	r.NoError(err)
	r.Empty(verified.Problems)
	report, err := repository.Restore(t.Context(), docbank.BackupRestoreOptions{
		SnapshotID: snapshot.ID, Target: filepath.Join(t.TempDir(), "recovered"),
	})
	r.NoError(err)
	r.Equal(snapshot.ID, report.SnapshotID)
	r.Equal(1, report.ExtrasFiles)
	restored, err := os.Open(filepath.Join(report.Target, "application", "recovery.bin"))
	r.NoError(err)
	t.Cleanup(func() { _ = restored.Close() })
	digest.Reset()
	n, err = io.Copy(digest, restored)
	r.NoError(err)
	r.NoError(restored.Close())
	r.Equal(size, n)
	r.Equal(wantHash, digest.Sum(nil))
}

func TestRepositoryRestoreAfterSourceLoss(t *testing.T) {
	for _, tc := range []struct {
		name   string
		driver sqlite.Driver
	}{
		{"default", nil},
		{"modernc", modernc.Driver{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			source := filepath.Join(t.TempDir(), "source")
			vault, err := docbank.New(t.Context(), docbank.Config{Root: source, SQLite: tc.driver})
			r.NoError(err)
			t.Cleanup(func() { r.NoError(vault.Close()) })
			body := []byte("synthetic archived content\n")
			createRangeFixture(t, vault, "/archive/document.txt", body)
			repositoryPath := filepath.Join(t.TempDir(), "repository")
			repository, err := docbank.InitBackupRepository(repositoryPath)
			r.NoError(err)
			extra := filepath.Join(t.TempDir(), "catalog.txt")
			r.NoError(os.WriteFile(extra, []byte("synthetic host catalog\n"), 0o600))
			snapshot, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{
				ExtraFiles: []docbank.BackupExtraFile{{Path: extra, RecordAs: "host/catalog.txt"}},
			})
			r.NoError(err)
			verified, err := repository.Verify(t.Context(), docbank.BackupVerifyOptions{SnapshotID: snapshot.ID})
			r.NoError(err)
			r.Empty(verified.Problems)
			r.NoError(vault.Close())
			r.NoError(os.RemoveAll(source))
			r.NoError(os.Remove(extra))
			repository, err = docbank.OpenBackupRepository(repositoryPath)
			r.NoError(err)
			report, err := repository.Restore(t.Context(), docbank.BackupRestoreOptions{
				SnapshotID: snapshot.ID, Target: filepath.Join(t.TempDir(), "recovered"),
				SQLite: tc.driver,
			})
			r.NoError(err)
			r.NoDirExists(source)
			r.True(report.Proof.ContentVerified)
			r.True(report.Proof.SQLiteIntegrity)
			r.Equal(1, report.ExtrasFiles)
			recovered, err := docbank.New(t.Context(), docbank.Config{Root: report.Target, SQLite: tc.driver})
			r.NoError(err)
			t.Cleanup(func() { r.NoError(recovered.Close()) })
			reader, err := recovered.OpenContent(t.Context(), "/archive/document.txt")
			r.NoError(err)
			got, err := io.ReadAll(reader.Reader)
			r.NoError(err)
			r.NoError(reader.Reader.Verify())
			r.NoError(reader.Reader.Close())
			r.Equal(body, got)
			host, err := os.ReadFile(filepath.Join(report.Target, "host", "catalog.txt"))
			r.NoError(err)
			r.Equal("synthetic host catalog\n", string(host))
		})
	}
}

func TestRepositoryRestoreEnforcesTargetBoundaries(t *testing.T) {
	r := require.New(t)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(t.TempDir(), "source")})
	r.NoError(err)
	t.Cleanup(func() { r.NoError(vault.Close()) })
	createRangeFixture(t, vault, "/archive/document.txt", []byte("archived\n"))
	repository, err := docbank.InitBackupRepository(filepath.Join(t.TempDir(), "repository"))
	r.NoError(err)
	_, err = vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{})
	r.NoError(err)
	protected := t.TempDir()
	activeRoot := filepath.Join(t.TempDir(), "active")
	active, err := docbank.New(t.Context(), docbank.Config{Root: activeRoot})
	r.NoError(err)
	t.Cleanup(func() { r.NoError(active.Close()) })
	for _, test := range []struct {
		name   string
		target string
		want   error
	}{
		{"repository", repository.Root(), docbank.ErrBackupRestoreTargetOverlap},
		{"protected", filepath.Join(protected, "target"), docbank.ErrBackupRestoreTargetOverlap},
		{"active", activeRoot, docbank.ErrBackupRestoreTargetActive},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := repository.Restore(t.Context(), docbank.BackupRestoreOptions{
				Target: test.target, ProtectedRoots: []string{protected}, Overwrite: true,
			})
			require.ErrorIs(t, err, test.want)
		})
	}
}
