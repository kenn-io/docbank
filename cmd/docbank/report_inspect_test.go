package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

func TestReportInspectionPreflight(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "absent-vault")
	t.Setenv("DOCBANK_HOME", vault)
	for _, args := range [][]string{
		{"show", "bad"}, {"show", strings.Repeat("A", 48)},
		{"show", strings.Repeat("z", 48)},
		{"history", "--offset", "-1"}, {"history", "--offset", "101"},
		{"history", "--limit", "0"}, {"history", "--limit", "51"},
	} {
		_, err := runCLI(t, append([]string{"search-export"}, args...)...)
		require.Error(t, err)
		require.Equal(t, exitUsage, commandExitCode(err, true), args)
		require.NoDirExists(t, vault)
	}
}

func createCLIReport(t *testing.T) (report.Summary, report.Request) {
	t.Helper()
	source := writeSourceFile(t, "synthetic-report.txt", "synthetic alpha")
	_, err := runCLI(t, "add", source, "--dest", "/")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		out, searchErr := runCLI(t, "search", "alpha")
		return searchErr == nil && strings.Contains(out, "/synthetic-report.txt")
	}, 15*time.Second, 25*time.Millisecond, "synthetic text was not searchable")
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	node, err := connection.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{
		Query: &apiclient.ResolvePathQuery{Path: "/synthetic-report.txt"},
	})
	require.NoError(t, err)
	request := report.Request{Version: 1, Timezone: "UTC", CoverageMode: "available_only",
		SelectedDocuments: &report.SelectedDocuments{Documents: []report.Identity{{
			NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash,
		}}},
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2020-01-01", End: "2100-01-01"}}},
	}
	summary, err := connection.CreateTermReport(t.Context(), request)
	require.NoError(t, err)
	return summary, request
}

func TestReportCLIInspection(t *testing.T) {
	setupVaultHome(t)
	summary, request := createCLIReport(t)
	output, err := runCLI(t, "search-export", "show", summary.ID, "--json")
	require.NoError(t, err)
	var shown report.Summary
	require.NoError(t, json.Unmarshal([]byte(output), &shown))
	require.Equal(t, summary, shown)
	output, err = runCLI(t, "search-export", "show", summary.ID)
	require.NoError(t, err)
	require.Contains(t, output, summary.ID)
	require.Contains(t, output, "coverage:")
	output, err = runCLI(t, "search-export", "history", "--limit", "1", "--json")
	require.NoError(t, err)
	var history store.TermReportHistoryPage
	require.NoError(t, json.Unmarshal([]byte(output), &history))
	require.Equal(t, 1, history.Total)
	require.Equal(t, []store.TermReportHistory{{Request: request, Summary: summary}}, history.Items)
	output, err = runCLI(t, "search-export", "history", "--offset", "100", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(output), &history))
	require.Empty(t, history.Items)
	require.Equal(t, 1, history.Total)
}

func TestReportCLIHistoryAfterRestart(t *testing.T) {
	vault := t.TempDir()
	t.Setenv("DOCBANK_HOME", vault)
	stop := startServe(t)
	waitForDaemon(t, vault)
	summary, _ := createCLIReport(t)
	stop()
	startServe(t)
	waitForDaemon(t, vault)
	output, err := runCLI(t, "search-export", "history", "--json")
	require.NoError(t, err)
	var page store.TermReportHistoryPage
	require.NoError(t, json.Unmarshal([]byte(output), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, summary.ID, page.Items[0].Summary.ID)
	_, err = runCLI(t, "search-export", "show", summary.ID)
	code, ok := daemonconn.ProblemCode(err)
	require.True(t, ok, "%v", err)
	require.Equal(t, "report_unavailable", code)
	require.Equal(t, exitGeneral, commandExitCode(err, true))
	destination := filepath.Join(t.TempDir(), "expired.zip")
	_, err = runCLI(t, "search-export", "download", summary.ID, "--output", destination)
	code, ok = daemonconn.ProblemCode(err)
	require.True(t, ok, "%v", err)
	require.Equal(t, "report_unavailable", code)
	require.Equal(t, exitGeneral, commandExitCode(err, true))
	require.ErrorContains(t, err, "create a new report")
	require.NotContains(t, err.Error(), "retrying")
	_, err = os.Stat(destination)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestReportSummaryOutput(t *testing.T) {
	summary := report.Summary{ID: strings.Repeat("a", 48), ParentID: strings.Repeat("b", 48),
		State: report.StateComplete, BundleBytes: 123, BundleSHA256: strings.Repeat("c", 64),
		Terms: []report.Term{{Number: 7, Expression: "alpha\nbeta"}},
		Counts: []report.Counts{{Hits: 1, HitsPlusFamily: 2, UniqueHits: 3,
			UniqueFamilies: 4, UniqueHitsPlusFamily: 5}},
		Coverage: report.Coverage{Warnings: []string{"synthetic missing evidence"}},
	}
	var output bytes.Buffer
	require.NoError(t, writeReportSummary(&output, summary))
	for _, want := range []string{summary.ID, "parent: " + summary.ParentID,
		`"alpha\nbeta"`, "Hits: 1", "Hits + family: 2", "Unique hits: 3",
		"Unique families: 4", "Unique hits + family: 5", "123 bytes",
		summary.BundleSHA256, "warning: synthetic missing evidence"} {
		require.Contains(t, output.String(), want)
	}
	require.NotContains(t, output.String(), "alpha\nbeta")
	summary.State, summary.Counts, summary.UnresolvedDates = "needs_review", nil, 2
	output.Reset()
	require.NoError(t, writeReportSummary(&output, summary))
	require.Contains(t, output.String(), "2 unresolved dates")
	require.Contains(t, output.String(), "counts and downloads are withheld")
	require.Contains(t, output.String(), "search-export dates "+summary.ID)
	require.NotContains(t, output.String(), "Hits:")
	reader, writer := io.Pipe()
	require.NoError(t, reader.Close())
	t.Cleanup(func() { _ = writer.Close() })
	require.ErrorIs(t, writeReportSummary(writer, summary), io.ErrClosedPipe)
}

func TestReportHistoryShortPageOutput(t *testing.T) {
	catalog, err := store.Open(filepath.Join(t.TempDir(), "history.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "strict"}
	for number := 1; number <= 128; number++ {
		request.Terms = append(request.Terms, report.Term{Number: number,
			Expression: strings.Repeat("🧪", 8192), Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2026-12-31"}})
	}
	observed := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	for _, id := range []string{strings.Repeat("b", 48), strings.Repeat("c", 48)} {
		require.NoError(t, catalog.SaveTermReportHistory(t.Context(), store.TermReportHistory{
			Request: request, Summary: report.Summary{ID: id, State: report.StateComplete,
				ObservedAt: observed, ExpiresAt: observed.Add(30 * time.Minute), Terms: request.Terms},
		}))
	}
	page, err := catalog.ListTermReportHistory(t.Context(), 0, 2)
	require.NoError(t, err)
	require.Equal(t, 2, page.Total)
	require.Len(t, page.Items, 1)
	var output bytes.Buffer
	require.NoError(t, writeReportHistory(&output, page, 0))
	require.Contains(t, output.String(), "--offset 1")
	require.Contains(t, output.String(), strings.Repeat("c", 48))
	require.Contains(t, output.String(), "recorded state: complete")
	require.Contains(t, output.String(), "does not retain downloadable artifacts")
	require.Contains(t, output.String(), "1 returned · 2 recorded runs total")
	output.Reset()
	require.NoError(t, writeReportHistory(&output, store.TermReportHistoryPage{Total: 2}, 100))
	require.Contains(t, output.String(), "0 returned")
	require.NotContains(t, output.String(), "Next page")
}
