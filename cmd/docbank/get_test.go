package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestGetPublishesVerifiedCurrentVersion(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "statement.bin", "synthetic binary\x00content")
	_, err := runCLI(t, "add", source, "--dest", "/inbox", "--json")
	require.NoError(t, err)

	output := filepath.Join(t.TempDir(), "retrieved.bin")
	out, err := runCLI(t, "get", "/inbox/statement.bin", output, "--json")
	require.NoError(t, err)
	var receipt getReceipt
	require.NoError(t, json.Unmarshal([]byte(out), &receipt), out)
	assert.Positive(t, receipt.NodeID)
	assert.NotEmpty(t, receipt.VersionID)
	assert.Len(t, receipt.BlobHash, 64)
	assert.Equal(t, int64(len("synthetic binary\x00content")), receipt.Size)
	absOutput, err := filepath.Abs(output)
	require.NoError(t, err)
	assert.Equal(t, absOutput, receipt.Output)
	bytes, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, "synthetic binary\x00content", string(bytes))
	assert.NotContains(t, out, "download:", "JSON must contain no progress lines")
	assertNoGetStaging(t, filepath.Dir(output))
}

func TestGetRefusesExistingDestinationUnlessOverwriteIsExplicit(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "report.txt", "authoritative")
	_, err := runCLI(t, "add", source, "--dest", "/inbox", "--json")
	require.NoError(t, err)

	output := filepath.Join(t.TempDir(), "report.txt")
	require.NoError(t, os.WriteFile(output, []byte("keep me"), 0o600))
	_, err = runCLI(t, "get", "/inbox/report.txt", output)
	require.ErrorContains(t, err, "pass --overwrite")
	bytes, readErr := os.ReadFile(output)
	require.NoError(t, readErr)
	assert.Equal(t, "keep me", string(bytes))
	assertNoGetStaging(t, filepath.Dir(output))

	out, err := runCLI(t, "get", "/inbox/report.txt", output, "--overwrite", "--json")
	require.NoError(t, err)
	assert.True(t, json.Valid([]byte(out)), out)
	bytes, readErr = os.ReadFile(output)
	require.NoError(t, readErr)
	assert.Equal(t, "authoritative", string(bytes))
	assertNoGetStaging(t, filepath.Dir(output))
}

func TestGetCanRetrieveTrashedFileByStableID(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "archived.txt", "retained bytes")
	_, err := runCLI(t, "add", source, "--dest", "/inbox", "--json")
	require.NoError(t, err)
	statJSON, err := runCLI(t, "stat", "/inbox/archived.txt", "--json")
	require.NoError(t, err)
	var node api.Node
	require.NoError(t, json.Unmarshal([]byte(statJSON), &node))
	_, err = runCLI(t, "rm", formatNodeSelector(node.ID), "--json")
	require.NoError(t, err)

	output := filepath.Join(t.TempDir(), "recovered.txt")
	_, err = runCLI(t, "get", formatNodeSelector(node.ID), output, "--json")
	require.NoError(t, err)
	bytes, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, "retained bytes", string(bytes))
}

func TestGetRejectsDirectoryAndInvalidProgress(t *testing.T) {
	_ = setupVaultHome(t)
	_, err := runCLI(t, "get", "/", filepath.Join(t.TempDir(), "root"))
	require.ErrorContains(t, err, "not a file")

	_, err = runCLI(t, "get", "/missing", filepath.Join(t.TempDir(), "out"),
		"--progress", "animated")
	require.ErrorContains(t, err, "invalid --progress value")
}

func TestPublishGetFileNoReplacePreservesConcurrentDestination(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "staged")
	output := filepath.Join(dir, "output")
	require.NoError(t, os.WriteFile(staged, []byte("download"), 0o600))
	require.NoError(t, os.WriteFile(output, []byte("concurrent"), 0o600))

	err := publishGetFile(staged, output, false)
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrExist)
	bytes, readErr := os.ReadFile(output)
	require.NoError(t, readErr)
	assert.Equal(t, "concurrent", string(bytes))
}

func TestGetIntegrityFailureNeverPublishesPartialDestination(t *testing.T) {
	dir := t.TempDir()
	staging, err := makePrivateStagingDirAt(dir, "docbank-get-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, staging.removeAll()) })
	content := "complete bytes without the required digest trailer"
	digest := sha256.Sum256([]byte(content))
	stream := &client.ContentStream{
		ReadCloser: io.NopCloser(strings.NewReader(content)),
		VersionID:  "7b0dbd90-5730-4436-b082-b692790725ff",
		BlobHash:   hex.EncodeToString(digest[:]),
		Size:       int64(len(content)),
	}
	output := filepath.Join(dir, "must-not-exist")
	renderer := newBackupProgressRenderer(io.Discard, backupProgressAuto)
	_, err = stageAndPublishGet(context.Background(), staging, stream, stream.Size,
		output, false, renderer)
	require.Error(t, err)
	require.ErrorIs(t, err, client.ErrIntegrity)
	_, statErr := os.Lstat(output)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func assertNoGetStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), "docbank-get-"), entry.Name())
	}
}
