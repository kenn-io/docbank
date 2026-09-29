package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
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
	for _, scope := range []string{"all", "null", "selected"} {
		t.Run(scope, func(t *testing.T) {
			_ = setupVaultHome(t)
			source := writeSourceFile(t, "synthetic-alpha.txt", "synthetic alpha")
			_, err := runCLI(t, "add", source, "--dest", "/")
			require.NoError(t, err)
			request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC",
				CoverageMode: "available_only", Terms: []report.Term{{Number: 1, Expression: "alpha",
					Syntax: "simple", Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}
			if scope == "selected" {
				other := writeSourceFile(t, "unselected-alpha.txt", "another alpha")
				_, err = runCLI(t, "add", other, "--dest", "/")
				require.NoError(t, err)
				client, err := daemonconn.Ensure(t.Context())
				require.NoError(t, err)
				node, err := client.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/synthetic-alpha.txt"}})
				require.NoError(t, err)
				request.AllDocuments = false
				request.SelectedDocuments = &report.SelectedDocuments{Documents: []report.Identity{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash}}}
			}
			encoded, err := json.Marshal(request)
			require.NoError(t, err)
			if scope == "null" {
				encoded = append(encoded[:len(encoded)-1], []byte(`,"selected_documents":null}`)...)
			}
			input := filepath.Join(t.TempDir(), "request.json")
			require.NoError(t, os.WriteFile(input, encoded, 0o600))
			packet := filepath.Join(t.TempDir(), "report.zip")
			output, err := runCLI(t, "search-export", "create", "--input", input, "--output", packet)
			require.NoError(t, err)
			require.Contains(t, output, "internally verified")
			_, err = runCLI(t, "search-export", "create", "--input", input, "--output", packet)
			require.ErrorContains(t, err, "--overwrite")

			if scope == "selected" {
				archive, err := zip.OpenReader(packet)
				require.NoError(t, err)
				for _, file := range archive.File {
					if file.Name == "manifest.json" {
						reader, err := file.Open()
						require.NoError(t, err)
						data, err := io.ReadAll(reader)
						require.NoError(t, err)
						require.NoError(t, reader.Close())
						var manifest struct {
							Request report.Request `json:"request"`
						}
						require.NoError(t, json.Unmarshal(data, &manifest))
						require.Equal(t, request.SelectedDocuments, manifest.Request.SelectedDocuments)
					}
				}
				require.NoError(t, archive.Close())
			}
			// The next commands must work with no path to a running vault.
			t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "not-a-vault"))
			output, err = runCLI(t, "search-export", "verify", packet)
			require.NoError(t, err)
			require.Contains(t, output, "internally consistent: true; source verified: false")
			csvPath := filepath.Join(t.TempDir(), "hits.csv")
			_, err = runCLI(t, "search-export", "csv", packet, "--output", csvPath)
			require.NoError(t, err)
			content, err := os.ReadFile(csvPath)
			require.NoError(t, err)
			require.Contains(t, string(content), "Unique Families")
			require.Contains(t, string(content), "alpha")
			rows, err := csv.NewReader(bytes.NewReader(content)).ReadAll()
			require.NoError(t, err)
			require.Len(t, rows, 2)
			require.Equal(t, "1", rows[1][3])
		})
	}
}
