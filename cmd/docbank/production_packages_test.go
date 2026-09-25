package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProductionPackagesCLIRejectsMissingJobAndLeavesNoFile(t *testing.T) {
	_ = setupVaultHome(t)
	const jobID = "78000000-0000-4000-8000-000000000071"
	const operationID = "78000000-0000-4000-8000-000000000072"
	_, err := runCLI(t, "production", "packages", "publish", jobID,
		"--profile", "export-dat-pdf-v1", "--operation-id", operationID)
	require.ErrorContains(t, err, "not found")

	parent := t.TempDir()
	destination := filepath.Join(parent, "synthetic-package.zip")
	_, err = runCLI(t, "production", "packages", "download", jobID, operationID, destination)
	require.ErrorContains(t, err, "not found")
	_, err = os.Stat(destination)
	require.ErrorIs(t, err, os.ErrNotExist)
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Empty(t, entries, "failed download must remove private staging")
}

func TestProductionPackagesCLIBoundsBeforeConnecting(t *testing.T) {
	const jobID = "78000000-0000-4000-8000-000000000071"
	_, err := runCLI(t, "production", "packages", "publish", jobID, "--profile", "unknown")
	require.ErrorContains(t, err, "invalid production package request")
	_, err = runCLI(t, "production", "packages", "publish", jobID,
		"--profile", "export-dat-pdf-v1", "--max-volume-documents", "0")
	require.ErrorContains(t, err, "invalid production package request")

	destination := filepath.Join(t.TempDir(), "existing.zip")
	require.NoError(t, os.WriteFile(destination, []byte("synthetic existing file"), 0o600))
	_, err = runCLI(t, "production", "packages", "download", jobID,
		"78000000-0000-4000-8000-000000000072", destination)
	require.ErrorContains(t, err, "pass --overwrite")
	data, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, "synthetic existing file", string(data))
}
