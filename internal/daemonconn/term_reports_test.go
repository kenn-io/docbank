package daemonconn_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"

	"go.kenn.io/docbank/internal/daemonconn"
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
	page, err := client.TermReportDates(t.Context(), summary.ID, report.DatePageRequest{MaxBytes: 64 << 10})
	require.NoError(t, err)
	require.Len(t, page.Members, 1)
	_, err = client.TermReportDates(t.Context(), summary.ID, report.DatePageRequest{MaxBytes: 1})
	require.ErrorIs(t, err, report.ErrReportLimit)
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

func TestReleaseTermReport(t *testing.T) {
	client, _ := newClient(t, serverKey)
	summary, err := client.CreateTermReport(t.Context(), report.Request{
		Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only",
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2024-12-31"}}},
	})
	require.NoError(t, err)
	require.NoError(t, client.ReleaseTermReport(t.Context(), summary.ID))
	_, err = client.GetTermReport(t.Context(), summary.ID)
	facts, ok := daemonconn.ExtractProblemFacts(err)
	require.True(t, ok)
	require.Equal(t, "report_unavailable", facts.Code)
	require.ErrorContains(t, err, "daemon error (410 report_unavailable)")
	err = client.ReleaseTermReport(t.Context(), summary.ID)
	facts, ok = daemonconn.ExtractProblemFacts(err)
	require.True(t, ok)
	require.Equal(t, "report_unavailable", facts.Code)
	require.ErrorContains(t, err, "daemon error (410 report_unavailable)")
}

func TestReleaseTermReportBoundary(t *testing.T) {
	id := strings.Repeat("a", 48)
	for _, status := range []int{204, 409, 410, 503, 404, 0} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/search-exports/"+id {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if status == 0 {
					panic(http.ErrAbortHandler)
				}
				if status == http.StatusNoContent {
					w.WriteHeader(status)
					return
				}
				code := "report_unavailable"
				if status == http.StatusConflict {
					code = "report_retained"
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"status":%d,"title":"Unavailable","code":%q}`, status, code)
			}))
			defer server.Close()
			client := daemonconn.New(server.URL, "synthetic-key")
			require.Error(t, client.ReleaseTermReport(t.Context(), "invalid"))
			require.Zero(t, calls.Load())
			err := client.ReleaseTermReport(t.Context(), id)
			require.EqualValues(t, 1, calls.Load())
			switch status {
			case http.StatusNoContent:
				require.NoError(t, err)
			case 0:
				require.True(t, daemonconn.IsTransportError(err), "%v", err)
			default:
				facts, ok := daemonconn.ExtractProblemFacts(err)
				require.True(t, ok)
				require.ErrorContains(t, err, fmt.Sprintf("daemon error (%d ", status))
				if status == http.StatusConflict {
					require.Equal(t, "report_retained", facts.Code)
				}
			}
		})
	}
}
