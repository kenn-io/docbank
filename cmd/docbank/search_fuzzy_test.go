package main

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestSearchFuzzyCLI(t *testing.T) {
	setupVaultHome(t)
	source := writeSourceFile(t, "invoice.txt", "synthetic accounts")
	_, err := runCLI(t, "add", source, "--dest", "/cases/acme")
	require.NoError(t, err)
	out, err := runCLI(t, "search", "invocie")
	require.NoError(t, err)
	require.NotContains(t, out, "invoice.txt")
	for _, query := range []string{"invocie", "cases/acme/invocie", "/cases/acme/invoice"} {
		out, err = runCLI(t, "search", query, "--fuzzy", "--under", "/cases", "--json")
		require.NoError(t, err, out)
		var report api.SearchReport
		require.NoError(t, json.Unmarshal([]byte(out), &report))
		require.Len(t, report.Hits, 1)
		require.Equal(t, "/cases/acme/invoice.txt", report.Hits[0].Path)
		require.NotNil(t, report.Fuzzy)
		require.NotNil(t, report.Fuzzy.Reasons)
		require.Equal(t, "complete", report.Fuzzy.Status)
	}
	out, err = runCLI(t, "search", "invoice", "--json")
	require.NoError(t, err)
	require.NotContains(t, out, `"fuzzy"`)
}

func TestSearchFuzzyRejectsProcessingFlags(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir()) // Validation must precede daemon startup.
	for _, flags := range [][]string{{"--mode", "lexical"}, {"--profile", ""}, {"--binding", ""}, {"--source-version", ""}, {"--explain=false"}, {"--rerank=false"}, {"--similar-to", "id:1"}} {
		args := append([]string{"search", "invocie", "--fuzzy"}, flags...)
		_, err := runCLI(t, args...)
		require.ErrorContains(t, err, "--fuzzy cannot be combined")
	}
	for _, flags := range [][]string{{}, {"--tag", "urgent"}, {"--modified-since", "2026-01-01T00:00:00Z"}} {
		_, err := runCLI(t, append([]string{"search", "--fuzzy"}, flags...)...)
		require.ErrorContains(t, err, "query")
	}
}

func TestSearchFuzzyEmptyLimitedOutput(t *testing.T) {
	for _, receipt := range []*api.FuzzyReceipt{nil, {Status: "complete", Reasons: []string{}}, {Status: "limited", Reasons: []string{"candidate_limit"}}, {Status: "not_run", Reasons: []string{"query_ineligible"}}} {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		report := api.SearchReport{Hits: []api.SearchHit{}, Limit: 10, Fuzzy: receipt}
		require.NoError(t, writeSearchReport(cmd, report, "", ""))
		require.Contains(t, out.String(), "no matches")
		if receipt != nil && receipt.Status != "complete" {
			require.Contains(t, out.String(), "incomplete")
			require.Contains(t, out.String(), "--under")
		} else {
			require.NotContains(t, out.String(), "incomplete")
		}
	}
}
