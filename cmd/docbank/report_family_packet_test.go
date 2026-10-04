package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json/v2"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/report"
)

type familyReportExpectation struct {
	counts      []report.Counts
	csv         [][]string
	members     []report.Identity
	edges       [][2]report.Identity
	familySizes []int
	incomplete  int64
}

func (f *familyReportFixture) assertReport(
	t *testing.T, observed familyReportObservation, want familyReportExpectation,
) {
	t.Helper()
	summary := observed.summary
	require.Equal(t, "complete", summary.State)
	require.Zero(t, summary.UnresolvedDates)
	require.Equal(t, want.counts, summary.Counts)
	require.Len(t, summary.RowCoverage, 2)
	for _, coverage := range append([]report.Coverage{summary.Coverage}, summary.RowCoverage...) {
		require.EqualValues(t, len(want.members), coverage.Scoped)
		require.EqualValues(t, len(want.members), coverage.Searchable)
		require.Zero(t, coverage.MissingText)
		require.Zero(t, coverage.FallbackDates)
		require.Equal(t, want.incomplete, coverage.IncompleteFamilies)
	}
	verifyFamilyPackets(t, observed.packet)
	z, err := zip.OpenReader(observed.packet)
	require.NoError(t, err)
	defer func() { require.NoError(t, z.Close()) }()
	var manifest struct {
		Counts            []report.Counts          `json:"counts"`
		Coverage          report.Coverage          `json:"coverage"`
		RowCoverage       []report.Coverage        `json:"row_coverage"`
		CoverageSelection report.CoverageSelection `json:"coverage_selection"`
	}
	raw, err := fs.ReadFile(z, "manifest.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &manifest))
	require.Equal(t, want.counts, manifest.Counts)
	require.Equal(t, summary.Coverage, manifest.Coverage)
	require.Equal(t, summary.RowCoverage, manifest.RowCoverage)
	require.Equal(t, f.profile, manifest.CoverageSelection.ProfileFingerprint)
	members := familyPacketRows[report.Member](t, z, "members.jsonl")
	var identities []report.Identity
	groups := make(map[string]int)
	var sizes []int
	for _, member := range members {
		identities = append(identities, member.Identity)
		if member.FamilyID == "" {
			sizes = append(sizes, 1) // Unconnected documents are separate singleton families.
		} else {
			groups[member.FamilyID]++
		}
		require.Equal(t, "complete", member.Coverage.SearchState)
		state := "complete"
		if want.incomplete != 0 {
			state = "incomplete"
		}
		require.Equal(t, state, member.Coverage.FamilyState)
	}
	require.ElementsMatch(t, want.members, identities)
	for _, count := range groups {
		sizes = append(sizes, count)
	}
	require.ElementsMatch(t, want.familySizes, sizes)
	var edges [][2]report.Identity
	for _, edge := range familyPacketRows[report.Relation](t, z, "families.jsonl") {
		edges = append(edges, [2]report.Identity{edge.Parent, edge.Child})
	}
	require.ElementsMatch(t, want.edges, edges)
	f.assertDates(t, z, members)
	assertFamilyCSV(t, z, observed.packet, want.csv)
}

func familyPacketRows[T any](t *testing.T, packet fs.FS, name string) []T {
	t.Helper()
	raw, err := fs.ReadFile(packet, name)
	require.NoError(t, err)
	var rows []T
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var row T
		require.NoError(t, json.Unmarshal([]byte(line), &row))
		rows = append(rows, row)
	}
	return rows
}

func (f *familyReportFixture) assertDates(t *testing.T, packet fs.FS, members []report.Member) {
	t.Helper()
	type datesRow struct {
		Document   report.Identity        `json:"document"`
		Candidates []report.DateCandidate `json:"candidates"`
		Texts      []report.TextBinding   `json:"texts"`
	}
	dates := familyPacketRows[datesRow](t, packet, "dates.jsonl")
	require.Len(t, dates, len(members))
	var identities []report.Identity
	for _, row := range dates {
		identities = append(identities, row.Document)
		require.Len(t, row.Texts, 1)
		require.Equal(t, row.Document, row.Texts[0].Document)
		require.Equal(t, f.profile, row.Texts[0].ProfileFingerprint)
	}
	var wanted []report.Identity
	for _, member := range members {
		wanted = append(wanted, member.Identity)
		require.Equal(t, "2024-05-06", member.Selection.Date)
		require.Equal(t, "content", member.Selection.Reason)
		require.Equal(t, "automatic", member.Selection.Mode)
		found := false
		for _, row := range dates {
			if row.Document != member.Identity {
				continue
			}
			for _, candidate := range row.Candidates {
				if candidate.ID == member.Selection.CandidateID {
					found = true
					require.Equal(t, "content", candidate.SourceClass)
					require.Equal(t, "document_date", candidate.Role)
					require.Equal(t, "2024-05-06", candidate.Value)
				}
			}
		}
		require.True(t, found, "selected candidate is retained")
	}
	require.ElementsMatch(t, wanted, identities)
}

func assertFamilyCSV(t *testing.T, packet fs.FS, path string, counts [][]string) {
	t.Helper()
	want := [][]string{{"Term #", "Terms", "Date Range", "Hits", "Hits Plus Family",
		"Unique Hits", "Unique Families", "Unique Hits Plus Family"}}
	for i, term := range []string{"alpha", "beta"} {
		want = append(want, append([]string{strconv.Itoa(i + 1), term,
			"2024-01-01 to 2024-12-31"}, counts[i]...))
	}
	raw, err := fs.ReadFile(packet, "hits.csv")
	require.NoError(t, err)
	rows, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	require.NoError(t, err)
	require.Equal(t, want, rows)
	output := filepath.Join(t.TempDir(), "counts.csv")
	_, err = runCLI(t, "search-export", "csv", path, "--output", output)
	require.NoError(t, err)
	raw, err = os.ReadFile(output)
	require.NoError(t, err)
	rows, err = csv.NewReader(bytes.NewReader(raw)).ReadAll()
	require.NoError(t, err)
	require.Equal(t, want, rows)
}
