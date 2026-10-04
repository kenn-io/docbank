package main

import (
	"encoding/json/v2"
	"path/filepath"
	"regexp"
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
