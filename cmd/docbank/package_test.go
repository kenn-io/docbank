package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPackagePreflightCLIUsesDaemonAndReturnsTypedResult(t *testing.T) {
	_ = setupVaultHome(t)
	root := t.TempDir()
	pdf, err := os.ReadFile(filepath.Join("..", "..", "document", "testdata", "scanassessment", "blank.pdf"))
	require.NoError(t, err)
	files := map[string][]byte{
		"VOL001/DATA/a.dat":        []byte("þDOCIDþ\x14þNATIVEþ\r\nþDOC-Aþ\x14þNATIVES/DOC-A.pdfþ\r\n"),
		"VOL001/NATIVES/DOC-A.pdf": pdf,
	}
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, contents, 0o600))
	}
	out, err := runCLI(t, "package", "preflight", root, "--profile", "dat-concordance-v1", "--encoding", "utf-8", "--json")
	require.NoError(t, err)
	var result api.PackagePreflight
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, 1, result.Records)
	assert.False(t, result.Blocking)
}

func TestPackagePreflightCLIRequiresDeclaredCodec(t *testing.T) {
	_, err := runCLI(t, "package", "preflight", "container-id")
	require.ErrorContains(t, err, "--profile is required")
}

func TestPackagePreflightSourceMakesDirectoryAbsolute(t *testing.T) {
	workingDirectory, err := os.Getwd()
	require.NoError(t, err)
	kind, reference, containerID, err := packagePreflightSource(".")
	require.NoError(t, err)
	assert.Equal(t, "root", kind)
	assert.Equal(t, workingDirectory, reference)
	assert.Empty(t, containerID)
}
