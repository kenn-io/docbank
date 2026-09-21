package daemonconn_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/report"
)

func TestTermReportClientReviewsAndDownloadsFrozenExport(t *testing.T) {
	client, catalog := newClient(t, serverKey)
	_, err := catalog.CreateFile(t.Context(), catalog.RootID(), "alpha.txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	summary, err := client.CreateTermReport(t.Context(), report.Request{
		Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only",
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2020-01-01", End: "2100-01-01"}}},
	})
	require.NoError(t, err)
	page, err := client.TermReportDates(t.Context(), summary.ID, report.DatePageRequest{})
	require.NoError(t, err)
	require.Len(t, page.Members, 1)
	candidate := page.Members[0].Candidates[0]
	revision, err := client.ReviseTermReport(t.Context(), summary.ID, []report.DateChoice{{
		Document: candidate.Document, CandidateID: candidate.ID, EvidenceSHA256: candidate.Locator.EvidenceSHA256,
		Reason: "Reviewed source", Action: "select",
	}})
	require.NoError(t, err)
	frozen, err := client.GetTermReport(t.Context(), revision.ID)
	require.NoError(t, err)
	require.Equal(t, revision, frozen)
	for _, format := range []string{"csv", "bundle"} {
		stream, err := client.OpenTermReport(t.Context(), frozen.ID, format)
		require.NoError(t, err)
		var output bytes.Buffer
		_, err = stream.CopyVerified(&output)
		require.NoError(t, stream.Close())
		require.NoError(t, err)
		if format == "csv" {
			require.Contains(t, output.String(), "Unique Families")
			continue
		}
		budget := report.NewBudget(8 << 20)
		verification, err := report.VerifyBundle(t.Context(), budget, bytes.NewReader(output.Bytes()), int64(output.Len()))
		require.NoError(t, budget.Close())
		require.NoError(t, err)
		require.True(t, verification.InternallyConsistent)
	}
}
