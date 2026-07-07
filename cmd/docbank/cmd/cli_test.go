package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCLI executes the root command against a test vault (caller must have
// set DOCBANK_HOME via t.Setenv) and returns captured stdout.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	err := rootCmd.ExecuteContext(t.Context())
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
