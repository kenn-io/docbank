package docbank_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank"
)

func TestRepositoryRestoreAfterSourceLoss(t *testing.T) {
	r := require.New(t)
	source := filepath.Join(t.TempDir(), "source")
	vault, err := docbank.New(t.Context(), docbank.Config{Root: source})
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
	r.NoError(vault.Close())
	r.NoError(os.RemoveAll(source))
	r.NoError(os.Remove(extra))
	repository, err = docbank.OpenBackupRepository(repositoryPath)
	r.NoError(err)
	report, err := repository.Restore(t.Context(), docbank.BackupRestoreOptions{
		SnapshotID: snapshot.ID, Target: filepath.Join(t.TempDir(), "recovered"),
	})
	r.NoError(err)
	r.NoDirExists(source)
	r.True(report.Proof.ContentVerified)
	r.True(report.Proof.SQLiteIntegrity)
	r.Equal(1, report.ExtrasFiles)
	recovered, err := docbank.New(t.Context(), docbank.Config{Root: report.Target})
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
