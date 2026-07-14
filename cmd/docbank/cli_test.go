package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

// runCLI executes the root command against a test vault (caller must have
// set DOCBANK_HOME via t.Setenv) and returns captured stdout.
//
// It must not pass t.Context(): cobra caches the first context on each
// subcommand (Command.ctx is only assigned when nil), so a later test would
// execute against an earlier test's canceled context.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetFlags(rootCmd)
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	err := rootCmd.ExecuteContext(context.Background())
	rootCmd.SetArgs(nil)
	return out.String(), err
}

// resetFlags restores every command's flags to their defaults. Package-level
// flag vars persist across in-process Execute calls, and a parse or argument
// validation failure exits before any command code could reset them, so the
// wrapper is the only reliable place.
func resetFlags(c *cobra.Command) {
	c.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Changed {
			if values, ok := f.Value.(pflag.SliceValue); ok && f.DefValue == "[]" {
				_ = values.Replace(nil)
			} else {
				_ = f.Value.Set(f.DefValue)
			}
			f.Changed = false
		}
	})
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

func setupVaultHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	startTestDaemon(t, dir)
	return dir
}

// startTestDaemon runs runServe in-process against dir and tears it down
// with the test. CLI commands discover it through the runtime record like
// production; no test-only transport exists.
func startTestDaemon(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("test daemon did not shut down")
		}
	})
	require.Eventually(t, func() bool {
		_, _, ok, err := client.Find(ctx, dir)
		return err == nil && ok
	}, 10*time.Second, 25*time.Millisecond, "test daemon never became ready")
}

func writeSourceFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func TestAddLsTreeCat(t *testing.T) {
	_ = setupVaultHome(t)
	src := writeSourceFile(t, "notes.txt", "hello vault")

	out, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "added: 1")

	out, err = runCLI(t, "ls", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "notes.txt")

	out, err = runCLI(t, "tree", "/")
	require.NoError(t, err)
	assert.Contains(t, out, "inbox")
	assert.Contains(t, out, "notes.txt")

	out, err = runCLI(t, "cat", "/inbox/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "hello vault", out)
}

func TestAddRerunReportsSkips(t *testing.T) {
	_ = setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")

	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	out, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "skipped: 1")
}

func TestAddPreflightReportsWithoutImporting(t *testing.T) {
	_ = setupVaultHome(t)
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "session.jsonl"), []byte("{\"ok\":true}\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(src, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".git", "config"), []byte("ignored"), 0o600))

	out, err := runCLI(t, "add", src, "--preflight", "--exclude", ".git")
	require.NoError(t, err)
	assert.Contains(t, out, "files: 1")
	assert.Contains(t, out, "excluded: 1")
	assert.Contains(t, out, ".jsonl")

	_, err = runCLI(t, "ls", "/inbox")
	require.Error(t, err, "preflight must not create the destination")

	out, err = runCLI(t, "add", src, "--preflight", "--exclude", ".git", "--json")
	require.NoError(t, err)
	var report api.IngestPreflightReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, int64(1), report.Files)
	assert.Equal(t, int64(1), report.Excluded)

	out, err = runCLI(t, "add", src, "--exclude", ".git")
	require.NoError(t, err)
	assert.Contains(t, out, "added: 1")
	assert.Contains(t, out, "excluded: 1")

	out, err = runCLI(t, "tree", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "session.jsonl")
	assert.NotContains(t, out, ".git")
}

func TestAddExcludeCommaIsLiteral(t *testing.T) {
	_ = setupVaultHome(t)
	src := t.TempDir()
	for _, rel := range []string{"cache,tmp/ignored.txt", "cache/kept.txt", "tmp/kept.txt"} {
		path := filepath.Join(src, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(rel), 0o600))
	}

	out, err := runCLI(t, "add", src, "--preflight", "--exclude", "cache,tmp", "--json")
	require.NoError(t, err)
	var report api.IngestPreflightReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, int64(2), report.Files,
		"comma-containing entry must be one literal rule; cache and tmp remain selected")
	assert.Equal(t, int64(1), report.Excluded)

	// The literal array flag appends repeated occurrences in production, but
	// in-process CLI executions must still reset it between invocations.
	out, err = runCLI(t, "add", src, "--preflight", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, int64(3), report.Files)
	assert.Zero(t, report.Excluded)
}

func TestAddMissingSourceFails(t *testing.T) {
	_ = setupVaultHome(t)

	_, err := runCLI(t, "add", "/no/such/file", "--dest", "/x")
	require.Error(t, err)
}

func TestCatRejectsDirectory(t *testing.T) {
	_ = setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	_, err = runCLI(t, "cat", "/inbox")
	require.Error(t, err)
}

func TestMvIntoDirAndRename(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	// Rename in place (dest is a non-existent name in an existing dir).
	_, err = runCLI(t, "mv", "/inbox/a.txt", "/inbox/b.txt")
	require.NoError(t, err)

	out, err := runCLI(t, "ls", "/inbox")
	require.NoError(t, err)
	assert.NotContains(t, out, "a.txt", "renamed source must be vacated")

	// Move into an existing directory, keeping the name.
	seed := writeSourceFile(t, "seed.txt", "s")
	_, err = runCLI(t, "add", seed, "--dest", "/filed")
	require.NoError(t, err)
	_, err = runCLI(t, "mv", "/inbox/b.txt", "/filed")
	require.NoError(t, err)

	out, err = runCLI(t, "ls", "/filed")
	require.NoError(t, err)
	assert.Contains(t, out, "b.txt")

	out, err = runCLI(t, "ls", "/inbox")
	require.NoError(t, err)
	assert.NotContains(t, out, "b.txt", "moved source must be vacated")
}

func TestMvOntoSelfFails(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	_, err = runCLI(t, "mv", "/inbox/a.txt", "/inbox/a.txt")
	require.ErrorIs(t, err, store.ErrExists)
}

func TestRmRestoreRoundTrip(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	// rm prints "trashed [<id>] <path> ..."; parse the id for restore.
	out, err := runCLI(t, "rm", "/inbox/a.txt")
	require.NoError(t, err)
	assert.Contains(t, out, "trashed")
	m := regexp.MustCompile(`\[(\d+)\]`).FindStringSubmatch(out)
	require.Len(t, m, 2)

	out, err = runCLI(t, "trash", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt")

	_, err = runCLI(t, "restore", m[1])
	require.NoError(t, err)
	out, err = runCLI(t, "ls", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt")
}

func TestSearchCLI(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "insurance-2026.txt", "policy")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "search", "insurance")
	require.NoError(t, err)
	assert.Contains(t, out, "/inbox/insurance-2026.txt")
}

func TestSearchCLIReportsTruncation(t *testing.T) {
	setupVaultHome(t)
	srcA := writeSourceFile(t, "report-a.txt", "alpha")
	srcB := writeSourceFile(t, "report-b.txt", "bravo")
	_, err := runCLI(t, "add", srcA, srcB, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "search", "report", "--limit", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "more than 1 result")
	assert.Contains(t, out, "increase --limit")

	_, err = runCLI(t, "search", "report", "--limit", "0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "between 1 and 1000")
}

func TestTrashEmpty(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	_, err = runCLI(t, "rm", "/inbox/a.txt")
	require.NoError(t, err)

	out, err := runCLI(t, "trash", "empty")
	require.NoError(t, err)
	assert.Contains(t, out, "1 trashed root")
	assert.Contains(t, out, "dry run")

	// Dry run leaves the node in trash.
	out, err = runCLI(t, "trash", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt")

	out, err = runCLI(t, "trash", "empty", "--run")
	require.NoError(t, err)
	assert.Contains(t, out, "deleted 1")

	out, err = runCLI(t, "trash", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "trash is empty")
}

func TestTreeRejectsFileWithoutOutput(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "tree", "/inbox/a.txt")
	require.ErrorIs(t, err, store.ErrNotDir)
	assert.Empty(t, out, "tree must not emit partial output before failing")
}

func TestTrashEmptyRejectsNegativeAge(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	_, err = runCLI(t, "rm", "/inbox/a.txt")
	require.NoError(t, err)

	_, err = runCLI(t, "trash", "empty", "--older-than=-1h")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative")

	// Nothing was deleted.
	out, err := runCLI(t, "trash", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt")
}

func TestGcReclaimsUnreachableBlobs(t *testing.T) {
	home := setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	// Nothing to collect while the node is live.
	out, err := runCLI(t, "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "0 candidate")

	_, err = runCLI(t, "rm", "/inbox/a.txt")
	require.NoError(t, err)
	_, err = runCLI(t, "trash", "empty", "--run")
	require.NoError(t, err)

	// Dry-run reports but does not delete.
	out, err = runCLI(t, "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "1 candidate")
	blobFiles, err := filepath.Glob(filepath.Join(home, "blobs", "??", "*"))
	require.NoError(t, err)
	assert.Len(t, blobFiles, 1)

	// --run deletes file and rows.
	out, err = runCLI(t, "gc", "--run")
	require.NoError(t, err)
	assert.Contains(t, out, "reclaimed")
	blobFiles, err = filepath.Glob(filepath.Join(home, "blobs", "??", "*"))
	require.NoError(t, err)
	assert.Empty(t, blobFiles)

	// Re-run converges.
	out, err = runCLI(t, "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "0 candidate")
}

func TestStorageStatusHumanAndJSON(t *testing.T) {
	_ = setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "storage", "status")
	require.NoError(t, err)
	assert.Contains(t, out, "loose: 1 blob(s), 5 byte(s)")
	assert.Contains(t, out, "packed: 0 live blob(s) in 0 pack(s)")

	out, err = runCLI(t, "storage", "status", "--json")
	require.NoError(t, err)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	assert.Equal(t, 1, status.LooseBlobs)
	assert.Equal(t, int64(5), status.LooseBytes)
}

func TestBackupInitCreateListVerifyCLI(t *testing.T) {
	home := t.TempDir()
	repoPath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[backup]\nrepo = \""+filepath.ToSlash(repoPath)+"\"\n"), 0o600))
	t.Setenv("DOCBANK_HOME", home)
	startTestDaemon(t, home)

	out, err := runCLI(t, "backup", "init")
	require.NoError(t, err)
	assert.Contains(t, out, "initialized backup repository")

	src := writeSourceFile(t, "archive.txt", "durable backup")
	_, err = runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	out, err = runCLI(t, "backup", "create", "--tag", "first", "--jobs", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "created snapshot")
	assert.Contains(t, out, "1 file(s), 1 blob(s)")
	for _, stage := range []string{"freeze:", "metadata:", "attachments:", "seal:"} {
		assert.Contains(t, out, stage)
	}

	out, err = runCLI(t, "backup", "create", "--tag", "json", "--jobs", "1", "--json")
	require.NoError(t, err)
	var created api.BackupSnapshot
	require.NoError(t, json.Unmarshal([]byte(out), &created), "--json must contain no progress lines")
	assert.Equal(t, "json", created.Tag)

	out, err = runCLI(t, "backup", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "SNAPSHOT")
	assert.Contains(t, out, "first")
	out, err = runCLI(t, "backup", "list", "--json")
	require.NoError(t, err)
	var listed api.BackupSnapshotList
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	require.Len(t, listed.Items, 2)
	assert.Equal(t, backupapp.MetadataFormat, listed.Items[0].MetadataFormat)

	out, err = runCLI(t, "backup", "verify", "--all", "--jobs", "1", "--progress", "plain")
	require.NoError(t, err)
	assert.Contains(t, out, "verify:")
	assert.Contains(t, out, "verified 2 snapshot(s)")
	assert.Contains(t, out, "0 problem(s)")

	out, err = runCLI(t, "backup", "verify", created.ID, "--quick", "--json")
	require.NoError(t, err)
	var verification api.BackupVerifyReport
	require.NoError(t, json.Unmarshal([]byte(out), &verification),
		"--json must contain no progress lines")
	assert.Equal(t, []string{created.ID}, verification.Snapshots)
	assert.Positive(t, verification.BytesRead, "quick verification still reads metadata")

	_, err = runCLI(t, "backup", "verify", created.ID, "--all")
	require.ErrorContains(t, err, "mutually exclusive")

	restoreTarget := filepath.Join(t.TempDir(), "human-restore")
	out, err = runCLI(t, "backup", "restore", created.ID, "--target", restoreTarget,
		"--jobs", "1", "--progress", "plain")
	require.NoError(t, err)
	for _, stage := range []string{
		"metadata:", "attachments:", "integrity:", "statistics:",
	} {
		assert.Contains(t, out, stage)
	}
	assert.Contains(t, out, "restored snapshot "+created.ID)
	assert.Contains(t, out, "1 packed in 1 pack(s), 0 loose")
	assert.Contains(t, out, "proof: content verified, SQLite integrity ok, manifest stats match")

	jsonRestoreTarget := filepath.Join(t.TempDir(), "json-restore")
	out, err = runCLI(t, "backup", "restore", "--target", jsonRestoreTarget,
		"--jobs", "1", "--json")
	require.NoError(t, err)
	var restoreReport api.BackupRestoreReport
	require.NoError(t, json.Unmarshal([]byte(out), &restoreReport),
		"--json must contain no progress lines")
	assert.Equal(t, created.ID, restoreReport.SnapshotID)
	targetInfo, err := os.Stat(jsonRestoreTarget)
	require.NoError(t, err)
	reportedTargetInfo, err := os.Stat(restoreReport.Target)
	require.NoError(t, err)
	assert.True(t, os.SameFile(targetInfo, reportedTargetInfo),
		"report target must identify the requested directory")
	assert.True(t, restoreReport.Proof.ContentVerified)
	assert.True(t, restoreReport.Proof.SQLiteIntegrity)
	assert.True(t, restoreReport.Proof.ManifestStats)

	overwriteTarget := filepath.Join(t.TempDir(), "overwrite-restore")
	require.NoError(t, os.MkdirAll(overwriteTarget, 0o700))
	marker := filepath.Join(overwriteTarget, "keep.txt")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0o600))
	_, err = runCLI(t, "backup", "restore", "--target", overwriteTarget, "--jobs", "1", "--json")
	require.ErrorContains(t, err, "not empty")
	markerBytes, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(markerBytes))
	out, err = runCLI(t, "backup", "restore", "--target", overwriteTarget,
		"--overwrite", "--jobs", "1", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &restoreReport))
	markerBytes, err = os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(markerBytes), "overwrite merges unrelated files")

	repo, err := backup.Open(repoPath)
	require.NoError(t, err)
	verified, err := backup.Verify(t.Context(), repo, backupapp.New("test"), backup.VerifyOptions{})
	require.NoError(t, err)
	assert.Empty(t, verified.Problems)

	manifests, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, manifests, 2)
	manifest := manifests[0]
	require.NotEmpty(t, manifest.NewPacks)
	packID := manifest.NewPacks[0]
	packPath := repo.Path("packs", packID[:2], packID+packstore.PackExt)
	packBytes, err := os.ReadFile(packPath)
	require.NoError(t, err)
	packBytes[len(packBytes)/3] ^= 1
	require.NoError(t, os.WriteFile(packPath, packBytes, 0o600))

	out, err = runCLI(t, "backup", "verify", manifest.SnapshotID, "--jobs", "1", "--json")
	require.ErrorContains(t, err, "found")
	require.NoError(t, json.Unmarshal([]byte(out), &verification),
		"failed JSON proof must remain one machine-readable report")
	assert.NotEmpty(t, verification.Problems)
	assert.Equal(t, manifest.SnapshotID, verification.Problems[0].SnapshotID)
}

func TestStoragePackBudgetAndJSON(t *testing.T) {
	_ = setupVaultHome(t)
	for name, content := range map[string]string{"a.txt": "alpha", "b.txt": "bravo"} {
		src := writeSourceFile(t, name, content)
		_, err := runCLI(t, "add", src, "--dest", "/inbox")
		require.NoError(t, err)
	}

	out, err := runCLI(t, "storage", "pack", "--max-bytes", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "packed 1 blob(s)")
	assert.Contains(t, out, "byte budget exhausted")

	out, err = runCLI(t, "storage", "pack", "--json")
	require.NoError(t, err)
	var report api.StoragePackReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.BlobsPacked)
	assert.False(t, report.BudgetExhausted)

	out, err = runCLI(t, "storage", "status", "--json")
	require.NoError(t, err)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	assert.Zero(t, status.LooseBlobs)
	assert.Equal(t, int64(2), status.PackedBlobs)
}

func TestStorageRepackJSON(t *testing.T) {
	_ = setupVaultHome(t)
	for name, content := range map[string]string{
		"keep.txt": "keep", "drop-a.txt": "drop a", "drop-b.txt": "drop b",
	} {
		src := writeSourceFile(t, name, content)
		_, err := runCLI(t, "add", src, "--dest", "/inbox")
		require.NoError(t, err)
	}
	_, err := runCLI(t, "storage", "pack")
	require.NoError(t, err)
	for _, path := range []string{"/inbox/drop-a.txt", "/inbox/drop-b.txt"} {
		_, err = runCLI(t, "rm", path)
		require.NoError(t, err)
	}
	_, err = runCLI(t, "trash", "empty", "--run")
	require.NoError(t, err)
	_, err = runCLI(t, "gc", "--run")
	require.NoError(t, err)

	out, err := runCLI(t, "storage", "repack", "--min-age", "1ns",
		"--min-dead-bytes", "1", "--json")
	require.NoError(t, err)
	var report api.StorageRepackReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.PacksRewritten)
	assert.Equal(t, 1, report.PacksRemoved)
	assert.Equal(t, 1, report.BlobsRepacked)

	out, err = runCLI(t, "storage", "status", "--json")
	require.NoError(t, err)
	var status api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(out), &status))
	assert.Zero(t, status.DeadPackedBytes)
}

func TestDeletionHelpSeparatesTrashGCAndRepack(t *testing.T) {
	out, err := runCLI(t, "rm", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "rm never permanently deletes metadata or reclaims content")

	out, err = runCLI(t, "trash", "empty", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "Content bytes remain")
	assert.Contains(t, out, "packed space then requires repack")

	out, err = runCLI(t, "gc", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "loose files are reclaimed immediately")
	assert.Contains(t, out, "requires a separate storage repack")
}

func TestVerifyDetectsMissingAndCorrupt(t *testing.T) {
	home := setupVaultHome(t)
	srcA := writeSourceFile(t, "a.txt", "alpha")
	srcB := writeSourceFile(t, "b.txt", "beta")
	_, err := runCLI(t, "add", srcA, srcB, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "verify")
	require.NoError(t, err)
	assert.Contains(t, out, "2 blob(s) ok")

	alphaSum := sha256.Sum256([]byte("alpha"))
	alphaHash := hex.EncodeToString(alphaSum[:])
	betaSum := sha256.Sum256([]byte("beta"))
	betaHash := hex.EncodeToString(betaSum[:])

	// Blobs are hash-named; find each by its expected hash rather than
	// assuming Glob's return order matches insertion order.
	blobFiles, err := filepath.Glob(filepath.Join(home, "blobs", "??", "*"))
	require.NoError(t, err)
	require.Len(t, blobFiles, 2)
	var alphaPath, betaPath string
	for _, p := range blobFiles {
		switch filepath.Base(p) {
		case alphaHash:
			alphaPath = p
		case betaHash:
			betaPath = p
		}
	}
	require.NotEmpty(t, alphaPath, "alpha blob not found among %v", blobFiles)
	require.NotEmpty(t, betaPath, "beta blob not found among %v", blobFiles)

	// Tamper alpha's blob, delete beta's.
	require.NoError(t, os.WriteFile(alphaPath, []byte("tampered"), 0o600))
	require.NoError(t, os.Remove(betaPath))

	out, err = runCLI(t, "verify")
	require.Error(t, err)
	assert.Contains(t, out, "corrupt: "+alphaHash)
	assert.Contains(t, out, "missing: "+betaHash)
}

// A validation failure exits before RunE, so nothing command-side can reset
// the parsed flag; the runCLI wrapper must prevent the stale --dest from
// leaking into the next invocation.
func TestAddDestDoesNotLeakAcrossValidationFailure(t *testing.T) {
	setupVaultHome(t)

	_, err := runCLI(t, "add", "--dest", "/leaked") // no sources: MinimumNArgs fails
	require.Error(t, err)

	src := writeSourceFile(t, "a.txt", "alpha")
	_, err = runCLI(t, "add", src) // no --dest: must use the default /inbox
	require.NoError(t, err)

	out, err := runCLI(t, "ls", "/inbox")
	require.NoError(t, err)
	assert.Contains(t, out, "a.txt")
	_, err = runCLI(t, "ls", "/leaked")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestGcReclaimsUntrackedBlobFiles(t *testing.T) {
	home := setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)

	// Manufacture the failed-transaction / crash shape: a durable blob file
	// with no blobs row. Row-based queries can never see it.
	content := []byte("orphaned bytes")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	shard := filepath.Join(home, "blobs", hash[:2])
	require.NoError(t, os.MkdirAll(shard, 0o700))
	orphan := filepath.Join(shard, hash)
	require.NoError(t, os.WriteFile(orphan, content, 0o600))

	// Dry run reports it without deleting.
	out, err := runCLI(t, "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "1 untracked")
	assert.FileExists(t, orphan)

	out, err = runCLI(t, "gc", "--run")
	require.NoError(t, err)
	assert.Contains(t, out, "reclaimed")
	assert.NoFileExists(t, orphan)

	// The tracked, live blob is untouched.
	catOut, err := runCLI(t, "cat", "/inbox/a.txt")
	require.NoError(t, err)
	assert.Equal(t, "alpha", catOut)
}
