package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestProvenanceCLIShowsHumanAndMachineAuthority(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "session.jsonl", `{"message":"synthetic"}`)
	_, err := runCLI(t, "add", source, "--dest", "/archive")
	require.NoError(t, err)

	out, err := runCLI(t, "provenance", "/archive/session.jsonl")
	require.NoError(t, err)
	assert.Contains(t, out, "Node:")
	assert.Contains(t, out, `"/archive/session.jsonl"`)
	assert.Contains(t, out, "Status:")
	assert.Contains(t, out, "active")
	assert.Contains(t, out, "Original path:")
	assert.Contains(t, out, filepath.Base(source))

	out, err = runCLI(t, "provenance", "/archive/session.jsonl", "--json")
	require.NoError(t, err)
	var page api.ProvenancePage
	require.NoError(t, json.Unmarshal([]byte(out), &page))
	assert.Equal(t, "/archive/session.jsonl", page.Node.Path)
	assert.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, source, page.Items[0].OriginalPath)
	assert.True(t, page.Items[0].Active)

	out, err = runCLI(t, "provenance", "/archive/session.jsonl", "--offset", "10")
	require.NoError(t, err)
	assert.Contains(t, out, "no facts at offset 10 (1 total)")
}

func TestProvenanceCLIValidatesPaginationBeforeDaemonStartup(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	_, err := runCLI(t, "provenance", "/document", "--limit", "0")
	require.ErrorContains(t, err, "--limit must be between")
	_, err = runCLI(t, "provenance", "/document", "--offset", "-1")
	require.ErrorContains(t, err, "--offset must not be negative")
}
