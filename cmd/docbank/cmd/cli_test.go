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

//nolint:unparam // kept for reuse in Tasks 15-16
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
