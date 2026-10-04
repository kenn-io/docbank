package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/report"
)

func TestReportFamilySelectedDocuments(t *testing.T) {
	f := newFamilyReportFixture(t, "selected")
	stop := f.start(t)
	client := startFamilyMCP(t)
	want := familyReportExpectation{
		counts: []report.Counts{
			{Hits: 1, HitsPlusFamily: 2, UniqueHits: 1, UniqueFamilies: 2, UniqueHitsPlusFamily: 2},
			{},
		},
		csv:     [][]string{{"1", "2", "1", "2", "2"}, {"0", "0", "0", "0", "0"}},
		members: []report.Identity{f.identity("P"), f.identity("C")},
		edges: [][2]report.Identity{
			{f.identity("P"), f.identity("C")}, {f.identity("P"), f.identity("U")},
		},
		familySizes: []int{2},
	}
	cli := captureFamilyCLI(t, f.request)
	f.assertReport(t, cli, want)
	mcp := client.captureReport(t, f.request)
	f.assertReport(t, mcp, want)
	client.stop(t)
	stop()
	verifyFamilyPackets(t, cli.packet, mcp.packet)
}

func TestReportFamilySharedChild(t *testing.T) {
	f := newFamilyReportFixture(t, "shared")
	stop := f.start(t)
	client := startFamilyMCP(t)
	want := familyReportExpectation{
		counts: []report.Counts{
			{Hits: 1, HitsPlusFamily: 2, UniqueHits: 1, UniqueFamilies: 0, UniqueHitsPlusFamily: 2},
			{Hits: 1, HitsPlusFamily: 2, UniqueHits: 1, UniqueFamilies: 0, UniqueHitsPlusFamily: 2},
		},
		csv:     [][]string{{"1", "2", "1", "0", "2"}, {"1", "2", "1", "0", "2"}},
		members: []report.Identity{f.identity("Q"), f.identity("R")},
		edges: [][2]report.Identity{
			{f.identity("Q"), f.identity("X")}, {f.identity("R"), f.identity("X")},
		},
		familySizes: []int{2},
	}
	cli := captureFamilyCLI(t, f.request)
	mcp := client.captureReport(t, f.request)
	for _, observed := range []familyReportObservation{cli, mcp} {
		f.assertReport(t, observed, want)
		warnings := strings.Join(observed.summary.Coverage.Warnings, "\n")
		require.Contains(t, warnings, f.identity("X").VersionID)
		require.Contains(t, warnings, "shared attachment child")
		require.Contains(t, warnings, "2 parents")
	}
	_, err := runCLI(t, "rm", formatNodeSelector(f.identity("X").NodeID))
	require.NoError(t, err)
	require.Equal(t, cli.summary, showReport(t, cli.summary.ID))
	require.Equal(t, mcp.summary, client.reportSummary(t, mcp.summary.ID))
	frozenCLI := filepath.Join(t.TempDir(), "frozen-cli.zip")
	_, err = runCLI(t, "search-export", "download", cli.summary.ID, "--output", frozenCLI)
	require.NoError(t, err)
	frozenMCP := filepath.Join(t.TempDir(), "frozen-mcp.zip")
	client.downloadReport(t, mcp.summary.ID, frozenMCP)
	for _, pair := range [][2]string{{cli.packet, frozenCLI}, {mcp.packet, frozenMCP}} {
		original, err := os.ReadFile(pair[0])
		require.NoError(t, err)
		frozen, err := os.ReadFile(pair[1])
		require.NoError(t, err)
		require.Equal(t, original, frozen, "trash must not change a frozen report")
	}
	available := f.request
	available.CoverageMode = "available_only"
	want.counts = []report.Counts{
		{Hits: 1, HitsPlusFamily: 1, UniqueHits: 1, UniqueFamilies: 1, UniqueHitsPlusFamily: 1},
		{Hits: 1, HitsPlusFamily: 1, UniqueHits: 1, UniqueFamilies: 1, UniqueHitsPlusFamily: 1},
	}
	want.csv = [][]string{{"1", "1", "1", "1", "1"}, {"1", "1", "1", "1", "1"}}
	want.edges, want.familySizes, want.incomplete = nil, []int{1, 1}, 2
	freshCLI := captureFamilyCLI(t, available)
	f.assertReport(t, freshCLI, want)
	freshMCP := client.captureReport(t, available)
	f.assertReport(t, freshMCP, want)
	assertFamilyRefused(t, client, f.request)
	client.stop(t)
	stop()
	verifyFamilyPackets(t, cli.packet, mcp.packet, frozenCLI, frozenMCP,
		freshCLI.packet, freshMCP.packet)
}

func TestReportFamilyIncompleteInventory(t *testing.T) {
	f := newFamilyReportFixture(t, "partial")
	stop := f.start(t)
	client := startFamilyMCP(t)
	available := f.request
	available.CoverageMode = "available_only"
	want := familyReportExpectation{
		counts: []report.Counts{
			{Hits: 1, HitsPlusFamily: 1, UniqueHits: 1, UniqueFamilies: 1, UniqueHitsPlusFamily: 1},
			{},
		},
		csv:         [][]string{{"1", "1", "1", "1", "1"}, {"0", "0", "0", "0", "0"}},
		members:     []report.Identity{f.identity("I")},
		familySizes: []int{1}, incomplete: 1,
	}
	cli := captureFamilyCLI(t, available)
	f.assertReport(t, cli, want)
	mcp := client.captureReport(t, available)
	f.assertReport(t, mcp, want)
	assertFamilyRefused(t, client, f.request)
	client.stop(t)
	stop()
	verifyFamilyPackets(t, cli.packet, mcp.packet)
}

func assertFamilyRefused(t *testing.T, client *familyMCP, request report.Request) {
	t.Helper()
	before := reportHistory(t)
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "strict.zip")
	out, err := runCLI(t, "search-export", "create", "--input",
		writeSourceFile(t, "strict.json", string(encoded)), "--output", destination)
	requireReportProblem(t, err, "incomplete_coverage")
	require.NotRegexp(t, `\b[0-9a-f]{48}\b`, out, "refusal must not return a report handle")
	require.NoFileExists(t, destination)
	require.Equal(t, before, reportHistory(t), "CLI refusal must not add history")
	raw := client.call(t, "create_report", map[string]any{"request": request})
	var failure struct {
		Code     string `json:"code"`
		ReportID string `json:"report_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &failure))
	require.Equal(t, "incomplete_coverage", failure.Code, string(raw))
	require.Empty(t, failure.ReportID)
	require.Equal(t, before, reportHistory(t), "MCP refusal must not add history")
}

type familyReportObservation struct {
	summary report.Summary
	packet  string
}

func captureFamilyCLI(t *testing.T, request report.Request) familyReportObservation {
	t.Helper()
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	packet := filepath.Join(t.TempDir(), "report.zip")
	out, err := runCLI(t, "search-export", "create", "--input",
		writeSourceFile(t, "request.json", string(encoded)), "--output", packet)
	require.NoError(t, err)
	id := regexp.MustCompile(`\b[0-9a-f]{48}\b`).FindString(out)
	require.NotEmpty(t, id, out)
	return familyReportObservation{summary: showReport(t, id), packet: packet}
}

func verifyFamilyPackets(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		out, err := runCLI(t, "search-export", "verify", path)
		require.NoError(t, err)
		require.Contains(t, out, "internally consistent: true; source verified: false")
	}
}
