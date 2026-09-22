package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/report"
)

func TestReportCoverageOutputShowsAvailableOnlyLimits(t *testing.T) {
	var output bytes.Buffer
	summary := report.Summary{
		Terms: []report.Term{{Number: 7, Expression: "synthetic alpha"}},
		Coverage: report.Coverage{Scoped: 10, Searchable: 8, MissingText: 2,
			IncompleteFamilies: 1, FallbackDates: 3, Warnings: []string{"synthetic coverage warning"}},
		RowCoverage: []report.Coverage{{Scoped: 10, Searchable: 7, MissingText: 2}},
	}
	require.NoError(t, writeReportCoverage(&output, summary))
	require.Contains(t, output.String(), "coverage: 10 scoped, 8 searchable, 2 missing text, 1 incomplete families, 3 fallback dates")
	require.Contains(t, output.String(), "counts exclude unavailable text or family evidence")
	require.Contains(t, output.String(), "term 7 coverage: 7 searchable")
	require.Contains(t, output.String(), "warning: synthetic coverage warning")
	output.Reset()
	require.NoError(t, writeReportCoverage(&output, report.Summary{State: "needs_review", UnresolvedDates: 2}))
	require.Contains(t, output.String(), "coverage pending date review")
	require.NotContains(t, output.String(), "0 scoped")
}

func TestReportCLIUsesDaemonThenVerifiesPacketOffline(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "synthetic-alpha.txt", "synthetic alpha")
	_, err := runCLI(t, "add", source, "--dest", "/")
	require.NoError(t, err)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC",
		CoverageMode: "available_only", Terms: []report.Term{{Number: 1, Expression: "alpha",
			Syntax: "simple", Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	input := filepath.Join(t.TempDir(), "request.json")
	require.NoError(t, os.WriteFile(input, encoded, 0o600))
	packet := filepath.Join(t.TempDir(), "report.zip")
	output, err := runCLI(t, "search-export", "create", "--input", input, "--output", packet)
	require.NoError(t, err)
	require.Contains(t, output, "internally verified")
	_, err = runCLI(t, "search-export", "create", "--input", input, "--output", packet)
	require.ErrorContains(t, err, "--overwrite")

	// The next commands must work with no path to a running vault.
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "not-a-vault"))
	output, err = runCLI(t, "search-export", "verify", packet)
	require.NoError(t, err)
	require.Contains(t, output, "internally consistent: true; source verified: false")
	csv := filepath.Join(t.TempDir(), "hits.csv")
	_, err = runCLI(t, "search-export", "csv", packet, "--output", csv)
	require.NoError(t, err)
	content, err := os.ReadFile(csv)
	require.NoError(t, err)
	require.Contains(t, string(content), "Unique Families")
	require.Contains(t, string(content), "alpha")
}
