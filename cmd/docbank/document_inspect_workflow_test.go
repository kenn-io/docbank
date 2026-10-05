package main

import (
	"encoding/json/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestDocumentInspectionWorkflow(t *testing.T) {
	f := newDocumentInspectionFixture(t)
	f.assertNoProcessingJobs(t)
	t.Setenv("DOCBANK_HOME", f.root)
	stop := startServe(t)
	waitForDaemon(t, f.root)
	node := f.nodes["notes.txt"]
	for name, state := range map[string]string{"notes.txt": "complete", "missing.txt": "unavailable"} {
		output, err := runCLI(t, "processing", "coverage", f.nodes[name].CurrentVersionID,
			"--profile", "archive", "--json")
		require.NoError(t, err)
		var got processingCoverageOutput
		require.NoError(t, json.Unmarshal([]byte(output), &got))
		require.Equal(t, f.nodes[name].CurrentVersionID, got.ContentVersionID.String())
		require.Equal(t, state, got.Coverage.Renditions.State)
		require.Equal(t, 1, got.Coverage.Renditions.Total)
	}
	output, err := runCLI(t, "processing", "coverage", uuid.New().String(), "--profile", "archive", "--json")
	require.NoError(t, err)
	var missing processingCoverageOutput
	require.NoError(t, json.Unmarshal([]byte(output), &missing))
	require.Equal(t, 1, missing.Coverage.Renditions.Stale)

	args := []string{"rendition", "window", "/notes.txt", "--version", node.CurrentVersionID,
		"--profile", "archive"}
	output, err = runCLI(t, append(args, "--json")...)
	require.NoError(t, err)
	var first api.RenditionTextWindow
	require.NoError(t, json.Unmarshal([]byte(output), &first))
	require.Equal(t, f.attachments["notes.txt"].ID, first.AttachmentID)
	require.Equal(t, f.attachments["notes.txt"].BuildID, first.BuildID)
	require.Equal(t, f.attachments["notes.txt"].Profile.Fingerprint, first.ProfileFingerprint)
	require.Contains(t, first.Text, "aé界🙂z")
	require.NotContains(t, first.Text, "other profile text")
	start := strings.Index(first.Text, "aé界🙂z")
	require.NotEqual(t, -1, start)
	offset := utf8.RuneCountInString(first.Text[:start]) + 1
	windowArgs := append(append([]string{}, args...), "--attachment", first.AttachmentID,
		"--offset", strconv.Itoa(offset), "--max-chars", "3")
	output, err = runCLI(t, append(windowArgs, "--json")...)
	require.NoError(t, err)
	var window api.RenditionTextWindow
	require.NoError(t, json.Unmarshal([]byte(output), &window))
	require.Equal(t, "é界🙂", window.Text)
	require.Equal(t, offset+3, window.NextOffset)
	output, err = runCLI(t, windowArgs...)
	require.NoError(t, err)
	_, continuation, ok := strings.Cut(output, "Next window: docbank ")
	require.True(t, ok)
	output, err = runCLI(t, append(strings.Fields(continuation), "--json")...)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(output), &window))
	require.True(t, strings.HasPrefix(window.Text, "z"))
	require.Equal(t, offset+3, window.ActualStart)
	for _, beyond := range []int{0, 1} {
		output, err = runCLI(t, append(args, "--attachment", first.AttachmentID,
			"--offset", strconv.Itoa(utf8.RuneCountInString(first.Text)+beyond), "--json")...)
		if beyond == 1 {
			require.ErrorContains(t, err, "invalid_rendition_window")
			require.Empty(t, output)
			continue
		}
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal([]byte(output), &window))
		require.True(t, window.EOF)
		require.Empty(t, window.Text)
	}
	stop()
	f.assertNoProcessingJobs(t)
}

func TestDocumentInspectionLimitsAndStaleSelection(t *testing.T) {
	f := newDocumentInspectionFixture(t)
	t.Setenv("DOCBANK_HOME", f.root)
	stop := startServe(t)
	waitForDaemon(t, f.root)
	longName := strings.Repeat("界", 256)
	long := f.nodes[longName]
	args := []string{"rendition", "window", formatNodeSelector(long.ID),
		"--version", long.CurrentVersionID, "--profile", "archive", "--json"}
	output, err := runCLI(t, args...)
	require.ErrorContains(t, err, "catalog filename limit")
	require.NotContains(t, err.Error(), "refresh")
	require.Empty(t, output)
	output, err = runCLI(t, append(args, "--attachment", f.attachments[longName].ID)...)
	require.NoError(t, err)
	require.Contains(t, output, "aé界🙂z")
	for _, command := range [][]string{
		{"processing", "coverage", f.nodes["portable.txt"].CurrentVersionID, "--profile", "portable"},
		{"rendition", "window", "/portable.txt", "--version", f.nodes["portable.txt"].CurrentVersionID,
			"--profile", "portable"},
	} {
		output, err = runCLI(t, command...)
		require.ErrorContains(t, err, "executable profile list")
		require.Empty(t, output)
	}
	_, err = runCLI(t, "rendition", "window", "/missing.txt", "--version",
		f.nodes["missing.txt"].CurrentVersionID, "--profile", "archive")
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	node := f.nodes["notes.txt"]
	args = []string{"rendition", "window", "/notes.txt", "--version", node.CurrentVersionID,
		"--profile", "archive", "--json"}
	_, err = runCLI(t, append(args, "--attachment", f.attachments["other"].ID)...)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	_, err = runCLI(t, "put", writeSourceFile(t, "replacement.txt", "replacement"),
		"/notes.txt", "--progress", "plain")
	require.NoError(t, err)
	output, err = runCLI(t, args...)
	require.ErrorContains(t, err, "refresh and reselect")
	require.Empty(t, output)
	output, err = runCLI(t, "processing", "coverage", node.CurrentVersionID,
		"--profile", "archive", "--json")
	require.NoError(t, err)
	var stale processingCoverageOutput
	require.NoError(t, json.Unmarshal([]byte(output), &stale))
	require.Equal(t, 1, stale.Coverage.Renditions.Stale)
	_, err = runCLI(t, "rm", formatNodeSelector(long.ID))
	require.NoError(t, err)
	_, err = runCLI(t, "rendition", "window", formatNodeSelector(long.ID),
		"--version", long.CurrentVersionID, "--profile", "archive", "--attachment", f.attachments[longName].ID)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	output, err = runCLI(t, "processing", "coverage", long.CurrentVersionID,
		"--profile", "archive", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(output), &stale))
	require.Equal(t, 1, stale.Coverage.Renditions.Stale)
	stop()
	f.assertNoProcessingJobs(t)
}

func TestDocumentInspectionRenditionReplacement(t *testing.T) {
	f := newDocumentInspectionFixture(t)
	t.Setenv("DOCBANK_HOME", f.root)
	stop := startServe(t)
	waitForDaemon(t, f.root)
	node := f.nodes["notes.txt"]
	args := []string{"rendition", "window", formatNodeSelector(node.ID),
		"--version", node.CurrentVersionID, "--profile", "archive", "--json"}
	_, err := runCLI(t, args...)
	require.NoError(t, err)
	stop()
	catalog, err := store.Open(filepath.Join(f.root, "docbank.db"))
	require.NoError(t, err)
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(f.root, "blobs"))
	require.NoError(t, err)
	replacement := f.publish(t, catalog, blobs, node, "archive", "replacement retained text")
	require.NoError(t, blobs.Close())
	// Enable audit before restarting to exercise vault identity with enrollment too.
	preview, err := catalog.PreviewInitialAudit(t.Context(), catalog.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = catalog.EnableInitialAudit(t.Context(), preview)
	require.NoError(t, err)
	require.NoError(t, catalog.Close())
	stop = startServe(t)
	waitForDaemon(t, f.root)
	output, err := runCLI(t, append(args, "--attachment", f.attachments["notes.txt"].ID,
		"--offset", "1")...)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	require.Empty(t, output)
	output, err = runCLI(t, args...)
	require.NoError(t, err)
	var window api.RenditionTextWindow
	require.NoError(t, json.Unmarshal([]byte(output), &window))
	require.Equal(t, replacement.ID, window.AttachmentID)
	require.Equal(t, replacement.BuildID, window.BuildID)
	require.Contains(t, window.Text, "replacement retained text")
	output, err = runCLI(t, "processing", "coverage", node.CurrentVersionID,
		"--profile", "archive", "--json")
	require.NoError(t, err)
	var coverage processingCoverageOutput
	require.NoError(t, json.Unmarshal([]byte(output), &coverage))
	require.Equal(t, window.VaultID, coverage.Coverage.VaultUID)
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	status, err := connection.API().AuditStatus(t.Context(), &apiclient.AuditStatusRequestOptions{})
	require.NoError(t, err)
	require.True(t, status.Enabled)
	require.NoError(t, connection.Close())
	stop()
	f.assertNoProcessingJobs(t)
}
