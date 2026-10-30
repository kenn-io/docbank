package main

import (
	"bytes"
	"encoding/json/v2"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/ingest"
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
	fetch := &ingest.URLFetch{Version: 1, RequestedURL: "https://example.com/download?id=7", FinalURL: "https://example.com/filing.txt", FetchedAt: "2026-10-01T12:34:56Z", ETag: []byte("W/\"edition-\xff\""), LastModified: []byte("Wed, 21 Oct 2015 07:28:00 GMT")}
	page.Items[0].URLFetch = fetch
	page.Items[0].SourceKind = "url"
	page.Items[0].OriginalPath = fetch.RequestedURL
	raw, err := json.Marshal(fetch)
	require.NoError(t, err)
	page.Items[0].SourceDescription = string(raw)
	var human bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&human)
	require.NoError(t, writeProvenance(cmd, page))
	assert.Contains(t, human.String(), "Requested URL:")
	assert.Contains(t, human.String(), "Final URL:")
	assert.Contains(t, human.String(), "Fetch time:")
	assert.NotContains(t, human.String(), "Source:")
	assert.Contains(t, human.String(), strconv.Quote(string(fetch.LastModified)))
	assert.Contains(t, human.String(), strconv.Quote(string(fetch.ETag)))
}

func TestProvenanceCLIValidatesPaginationBeforeDaemonStartup(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	_, err := runCLI(t, "provenance", "/document", "--limit", "0")
	require.ErrorContains(t, err, "--limit must be between")
	_, err = runCLI(t, "provenance", "/document", "--offset", "-1")
	require.ErrorContains(t, err, "--offset must not be negative")
}
