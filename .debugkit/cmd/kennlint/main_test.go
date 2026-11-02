package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/lint/config"
)

func TestConfigWritesThenChecksClean(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	out := filepath.Join(dir, ".golangci.yml")
	overlay := filepath.Join(dir, ".golangci.overlay.yml")
	require.NoError(os.WriteFile(overlay, []byte("run:\n  timeout: 3m\n"), 0o644))

	var stdout, stderr bytes.Buffer
	require.NoError(runConfig([]string{"-overlay", overlay, "-out", out}, &stdout, &stderr))
	assert.Equal("wrote "+out+"\n", stdout.String())

	written, err := os.ReadFile(out)
	require.NoError(err)
	assert.Contains(string(written), "timeout: 3m")
	assert.True(bytes.HasPrefix(written, []byte(config.Header)))

	assert.NoError(runConfig([]string{"-overlay", overlay, "-out", out, "-check"}, &stdout, &stderr))
}

func TestConfigCheckReportsDrift(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	out := filepath.Join(dir, ".golangci.yml")
	require.NoError(t, os.WriteFile(out, []byte("version: \"2\"\n"), 0o644))

	err := runConfig([]string{"-overlay", filepath.Join(dir, "missing.yml"), "-out", out, "-check"}, &bytes.Buffer{}, &bytes.Buffer{})
	require.ErrorIs(t, err, errDrift)
	assert.ErrorContains(err, "kennlint config")
}

func TestConfigMissingOverlayRendersCanonical(t *testing.T) {
	var stdout bytes.Buffer
	dir := t.TempDir()
	require.NoError(t, runConfig([]string{"-overlay", filepath.Join(dir, "absent.yml"), "-out", "-"}, &stdout, &bytes.Buffer{}))
	want, err := config.Render(nil)
	require.NoError(t, err)
	assert.Equal(t, string(want), stdout.String())
}

func TestConfigRejectsPositionalArguments(t *testing.T) {
	err := runConfig([]string{"extra"}, &bytes.Buffer{}, &bytes.Buffer{})
	assert.ErrorContains(t, err, "unexpected arguments")
}

func TestSQLReportsCheckConstraintsInSQLFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dir, "migrations"), 0o755))
	require.NoError(os.WriteFile(filepath.Join(dir, "migrations", "0001_init.up.sql"),
		[]byte("CREATE TABLE jobs (\n  status TEXT CHECK (status IN ('a', 'b')),\n  n INTEGER CHECK (n > 0)\n);\n"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("CHECK (x IN ('ignored'))"), 0o644))

	var stdout bytes.Buffer
	n, err := runSQL([]string{dir}, &stdout)
	require.NoError(err)
	assert.Equal(2, n)
	assert.Contains(stdout.String(), "migrations/0001_init.up.sql:2:15: CHECK constraint on status locks")
	assert.Contains(stdout.String(), "migrations/0001_init.up.sql:3:13: CHECK constraint on n locks")

	stdout.Reset()
	n, err = runSQL([]string{filepath.Join(dir, "missing")}, &stdout)
	require.Error(err)
	assert.Zero(n)
}
