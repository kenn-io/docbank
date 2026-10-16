package main

import (
	"context"
	"encoding/csv"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
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

func reportRecoveryArgs(t *testing.T, revise bool) ([]string, string) {
	t.Helper()
	setupVaultHome(t)
	text := "Alpha. Document dated 2024-05-06."
	if revise {
		text += " Document dated 2024-06-07."
	}
	source := writeSourceFile(t, "dated.txt", text)
	_, err := runCLI(t, "add", source, "--dest", "/")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		out, searchErr := runCLI(t, "search", "alpha")
		return searchErr == nil && strings.Contains(out, "/dated.txt")
	}, 15*time.Second, 25*time.Millisecond, "synthetic text was not searchable")
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC",
		CoverageMode: "available_only", Terms: []report.Term{{Number: 1, Expression: "alpha",
			Syntax: "simple", Dates: report.DateRange{Start: "2024-01-01", End: "2024-12-31"}}}}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	input := writeSourceFile(t, "request.json", string(encoded))
	if !revise {
		return []string{"create", "--input", input}, ""
	}
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	parent, err := connection.CreateTermReport(t.Context(), request)
	require.NoError(t, err)
	page, err := connection.TermReportDates(t.Context(), parent.ID, report.DatePageRequest{})
	require.NoError(t, err)
	require.Equal(t, "needs_review", parent.State)
	require.Len(t, page.Members, 1)
	var choice report.DateChoice
	for _, candidate := range page.Members[0].Candidates {
		if candidate.Value == "2024-05-06" && candidate.Role == "document_date" {
			choice = report.DateChoice{Document: candidate.Document, CandidateID: candidate.ID,
				EvidenceSHA256: candidate.Locator.EvidenceSHA256, Action: "select",
				Reason: "Reviewed synthetic document date"}
			break
		}
	}
	require.NotEmpty(t, choice.CandidateID)
	encoded, err = json.Marshal([]report.DateChoice{choice})
	require.NoError(t, err)
	choices := writeSourceFile(t, "choices.json", string(encoded))
	return []string{"revise", parent.ID, "--choices", choices}, parent.ID
}

func recordedCLIResult(t *testing.T, parentID string) report.Summary {
	t.Helper()
	output, err := runCLI(t, "search-export", "history", "--json")
	require.NoError(t, err)
	var page store.TermReportHistoryPage
	require.NoError(t, json.Unmarshal([]byte(output), &page))
	wantTotal := 1
	if parentID != "" {
		wantTotal = 2
	}
	require.Equal(t, wantTotal, page.Total)
	for _, item := range page.Items {
		if item.Summary.ParentID == parentID {
			return item.Summary
		}
	}
	t.Fatal("created report missing from history")
	return report.Summary{}
}

func TestReportCLIRecoversKnownResult(t *testing.T) {
	for _, operation := range []string{"create", "revise"} {
		t.Run(operation, func(t *testing.T) {
			args, parentID := reportRecoveryArgs(t, operation == "revise")
			if parentID != "" {
				output, err := runCLI(t, "search-export", "show", parentID)
				require.NoError(t, err)
				require.Contains(t, output, "counts and downloads are withheld")
				destination := filepath.Join(t.TempDir(), "unreviewed.zip")
				_, err = runCLI(t, "search-export", "download", parentID, "--output", destination)
				require.ErrorIs(t, err, daemonconn.ErrReportReviewRequired)
				require.Equal(t, exitGeneral, commandExitCode(err, true))
				require.NoFileExists(t, destination)
			}
			syncErr := errors.New("synthetic directory sync failure")
			originalSync := syncGetDestinationDir
			syncGetDestinationDir = func(string) error { return syncErr }
			t.Cleanup(func() { syncGetDestinationDir = originalSync })
			destination := filepath.Join(t.TempDir(), "saved.zip")
			args = append(args, "--output", destination)
			_, err := runCLI(t, append([]string{"search-export"}, args...)...)
			require.ErrorIs(t, err, syncErr)
			created := recordedCLIResult(t, parentID)
			require.ErrorContains(t, err, created.ID)
			require.ErrorContains(t, err, "inspect")
			require.ErrorContains(t, err, "file published")
			if parentID != "" {
				require.NotContains(t, err.Error(), parentID)
			}
			syncGetDestinationDir = originalSync
			destination = filepath.Join(t.TempDir(), "recovered.zip")
			_, err = runCLI(t, "search-export", "download", created.ID, "--output", destination)
			require.NoError(t, err)
			_, err = verifyReportFile(t.Context(), destination)
			require.NoError(t, err)
			require.Equal(t, created, recordedCLIResult(t, parentID))
		})
	}
}

type reportTestWriter func([]byte) (int, error)

func (write reportTestWriter) Write(p []byte) (int, error) { return write(p) }

func TestReportCLIKnownResultOutputErrors(t *testing.T) {
	for _, operation := range []string{"create", "revise"} {
		for _, phase := range []string{"coverage", "saved"} {
			t.Run(operation+"/"+phase, func(t *testing.T) {
				args, parentID := reportRecoveryArgs(t, operation == "revise")
				destination := filepath.Join(t.TempDir(), "saved.zip")
				resetFlags(rootCmd)
				rootCmd.SetArgs(append([]string{"search-export"},
					append(args, "--output", destination)...))
				rootCmd.SetOut(reportTestWriter(func(p []byte) (int, error) {
					_, statErr := os.Stat(destination)
					if phase == "coverage" || statErr == nil {
						return 0, io.ErrClosedPipe
					}
					return len(p), nil
				}))
				rootCmd.SetErr(io.Discard)
				t.Cleanup(func() {
					rootCmd.SetArgs(nil)
					rootCmd.SetOut(nil)
					rootCmd.SetErr(nil)
				})
				err := rootCmd.ExecuteContext(context.Background())
				require.ErrorIs(t, err, io.ErrClosedPipe)
				created := recordedCLIResult(t, parentID)
				require.ErrorContains(t, err, created.ID)
				if parentID != "" {
					require.NotContains(t, err.Error(), parentID)
				}
				if phase == "saved" {
					require.ErrorContains(t, err, "verified file is saved")
					_, err = verifyReportFile(t.Context(), destination)
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "search-export show "+created.ID)
					require.NoFileExists(t, destination)
				}
			})
		}
	}
}
