package main

import (
	"encoding/csv"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestReportCLIDownloadByID(t *testing.T) {
	setupVaultHome(t)
	summary, _ := createCLIReport(t)
	t.Chdir(t.TempDir())
	output, err := runCLI(t, "search-export", "download", summary.ID, "--output", "report.zip")
	require.NoError(t, err)
	require.Contains(t, output, summary.ID)
	verification, err := verifyReportFile(t.Context(), "report.zip")
	require.NoError(t, err)
	require.True(t, verification.InternallyConsistent)
	require.False(t, verification.SourceVerified)
	_, err = runCLI(t, "search-export", "csv", "report.zip", "--output", "counts.csv")
	require.NoError(t, err)
	content, err := os.ReadFile("counts.csv")
	require.NoError(t, err)
	rows, err := csv.NewReader(strings.NewReader(string(content))).ReadAll()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "1", rows[1][3])

	require.NoError(t, os.WriteFile("report.zip", []byte("keep this file"), 0o600))
	_, err = runCLI(t, "search-export", "download", summary.ID, "--output", "report.zip")
	require.ErrorContains(t, err, "--overwrite")
	content, err = os.ReadFile("report.zip")
	require.NoError(t, err)
	require.Equal(t, "keep this file", string(content))
	_, err = runCLI(t, "search-export", "download", summary.ID,
		"--output", "report.zip", "--overwrite")
	require.NoError(t, err)
	_, err = verifyReportFile(t.Context(), "report.zip")
	require.NoError(t, err)
	output, err = runCLI(t, "search-export", "history", "--json")
	require.NoError(t, err)
	var page store.TermReportHistoryPage
	require.NoError(t, json.Unmarshal([]byte(output), &page))
	require.Equal(t, 1, page.Total)
	require.Equal(t, summary.ID, page.Items[0].Summary.ID)
}

func TestReportDownloadPreflight(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "absent-vault")
	t.Setenv("DOCBANK_HOME", vault)
	id := strings.Repeat("a", 48)
	existing := writeSourceFile(t, "existing.zip", "keep me")
	for _, args := range [][]string{
		{"bad", "--output", filepath.Join(t.TempDir(), "report.zip")}, {id},
		{id, "--output", existing},
		{id, "--output", filepath.Join(t.TempDir(), "missing", "report.zip")},
	} {
		_, err := runCLI(t, append([]string{"search-export", "download"}, args...)...)
		require.Error(t, err)
		require.Equal(t, exitUsage, commandExitCode(err, true), args)
		require.NoDirExists(t, vault)
	}
}

func TestReportDeliveryErrors(t *testing.T) {
	id := strings.Repeat("a", 48)
	for _, tc := range []struct {
		cause error
		code  int
		hint  string
	}{
		{daemonconn.ErrReportReviewRequired, exitGeneral, "search-export dates " + id},
		{io.ErrUnexpectedEOF, exitGeneral, "inspect"},
		{daemonconn.ErrIntegrity, exitIntegrity, "inspect"},
	} {
		err := reportDeliveryError(id, "report.zip", tc.cause)
		require.ErrorIs(t, err, tc.cause)
		require.ErrorContains(t, err, id)
		require.ErrorContains(t, err, tc.hint)
		require.Equal(t, tc.code, commandExitCode(err, true))
	}
}

func TestReportDownloadOutputFailure(t *testing.T) {
	setupVaultHome(t)
	summary, _ := createCLIReport(t)
	reader, writer := io.Pipe()
	require.NoError(t, reader.Close())
	t.Cleanup(func() { _ = writer.Close() })
	cmd := newReportDownloadCommand()
	cmd.SetOut(writer)
	cmd.SetErr(io.Discard)
	output := filepath.Join(t.TempDir(), "saved.zip")
	cmd.SetArgs([]string{summary.ID, "--output", output})
	err := cmd.ExecuteContext(t.Context())
	require.ErrorIs(t, err, io.ErrClosedPipe)
	require.ErrorContains(t, err, "verified file is saved")
	require.ErrorContains(t, err, summary.ID)
	_, err = verifyReportFile(t.Context(), output)
	require.NoError(t, err)
}
