package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/home"
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
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	err := rootCmd.ExecuteContext(context.Background())
	rootCmd.SetArgs(nil)
	return out.String(), err
}

func setupVaultHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	return dir
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

	// Move into an existing directory, keeping the name.
	seed := writeSourceFile(t, "seed.txt", "s")
	_, err = runCLI(t, "add", seed, "--dest", "/filed")
	require.NoError(t, err)
	_, err = runCLI(t, "mv", "/inbox/b.txt", "/filed")
	require.NoError(t, err)

	out, err := runCLI(t, "ls", "/filed")
	require.NoError(t, err)
	assert.Contains(t, out, "b.txt")
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

func TestTrashEmpty(t *testing.T) {
	setupVaultHome(t)
	src := writeSourceFile(t, "a.txt", "alpha")
	_, err := runCLI(t, "add", src, "--dest", "/inbox")
	require.NoError(t, err)
	_, err = runCLI(t, "rm", "/inbox/a.txt")
	require.NoError(t, err)

	out, err := runCLI(t, "trash", "empty")
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

func TestOpenVaultSkipsTmpCleanupWhileVaultBusy(t *testing.T) {
	dir := setupVaultHome(t)
	_, err := runCLI(t, "ls", "/") // create the vault layout
	require.NoError(t, err)
	stale := filepath.Join(dir, "blobs", "tmp", "blob-stale")
	require.NoError(t, os.WriteFile(stale, []byte("x"), 0o600))

	// Another holder (simulating a concurrent docbank process mid-write)
	// must prevent startup cleanup from deleting its temp file.
	other, err := home.Layout{Root: dir}.AcquireLock(false)
	require.NoError(t, err)
	_, err = runCLI(t, "ls", "/")
	require.NoError(t, err)
	assert.FileExists(t, stale, "cleanup must be skipped while the vault is shared")

	// Sole process again: startup cleanup reclaims the stale file.
	require.NoError(t, other.Release())
	_, err = runCLI(t, "ls", "/")
	require.NoError(t, err)
	assert.NoFileExists(t, stale)
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
	_, err = runCLI(t, "trash", "empty")
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

func TestVerifyDetectsMissingAndCorrupt(t *testing.T) {
	home := setupVaultHome(t)
	srcA := writeSourceFile(t, "a.txt", "alpha")
	srcB := writeSourceFile(t, "b.txt", "beta")
	_, err := runCLI(t, "add", srcA, srcB, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "verify")
	require.NoError(t, err)
	assert.Contains(t, out, "2 blob(s) ok")

	// Corrupt one blob file, delete the other.
	blobFiles, err := filepath.Glob(filepath.Join(home, "blobs", "??", "*"))
	require.NoError(t, err)
	require.Len(t, blobFiles, 2)
	require.NoError(t, os.WriteFile(blobFiles[0], []byte("tampered"), 0o600))
	require.NoError(t, os.Remove(blobFiles[1]))

	out, err = runCLI(t, "verify")
	require.Error(t, err)
	assert.Contains(t, out, "corrupt")
	assert.Contains(t, out, "missing")
}
