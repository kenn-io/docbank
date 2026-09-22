package store

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/report"
)

func TestTermReportHistoryRetainsReusableRequestAcrossMetadataRoundTrip(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	now := time.Date(2026, 9, 20, 15, 30, 0, 0, time.UTC)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "strict",
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2026-12-31"}}}}
	item := TermReportHistory{Request: request, Summary: report.Summary{
		ID: strings.Repeat("a", 48), State: report.StateComplete, ObservedAt: now, ExpiresAt: now.Add(30 * time.Minute),
		Terms: request.Terms, Counts: []report.Counts{{Hits: 2}},
	}}
	require.NoError(t, s.SaveTermReportHistory(ctx, item))
	page, err := s.ListTermReportHistory(ctx, 0, 50)
	require.NoError(t, err)
	require.Equal(t, 1, page.Total)
	require.Equal(t, item, page.Items[0])

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	require.Contains(t, exported.String(), `"type":"term_report_history"`)
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	restoredPage, err := restored.ListTermReportHistory(ctx, 0, 50)
	require.NoError(t, err)
	require.Equal(t, page, restoredPage)
}

func TestTermReportHistoryBoundsRetentionAcrossRestore(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "strict",
		Terms: []report.Term{{Number: 1, Expression: "synthetic", Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2026-12-31"}}}}
	start := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	for i := range 101 {
		now := start.Add(time.Duration(i) * time.Second)
		receipt := TermReportHistory{Request: request, Summary: report.Summary{
			ID: fmt.Sprintf("%048x", i+1), State: report.StateComplete, ObservedAt: now,
			ExpiresAt: now.Add(30 * time.Minute), Terms: request.Terms,
		}}
		require.NoError(t, s.SaveTermReportHistory(ctx, receipt))
	}
	page, err := s.ListTermReportHistory(ctx, 0, 50)
	require.NoError(t, err)
	require.Equal(t, 100, page.Total)
	require.Len(t, page.Items, 50)
	require.Equal(t, fmt.Sprintf("%048x", 101), page.Items[0].Summary.ID)
	last, err := s.ListTermReportHistory(ctx, 50, 50)
	require.NoError(t, err)
	require.Len(t, last.Items, 50)
	require.Equal(t, fmt.Sprintf("%048x", 2), last.Items[49].Summary.ID)
	// The bounded request and receipt list is portable metadata.
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	restoredPage, err := restored.ListTermReportHistory(ctx, 0, 50)
	require.NoError(t, err)
	require.Equal(t, page, restoredPage)
}

func TestTermReportHistoryAcceptsMaximumValidTermText(t *testing.T) {
	s := newTestStore(t)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "strict"}
	for number := 1; number <= 128; number++ {
		request.Terms = append(request.Terms, report.Term{Number: number,
			Expression: strings.Repeat("🧪", 8192), Syntax: "simple",
			Dates: report.DateRange{Start: "2024-01-01", End: "2026-12-31"}})
	}
	_, err := report.NormalizeRequest(request)
	require.NoError(t, err)
	now := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	item := TermReportHistory{Request: request, Summary: report.Summary{
		ID: strings.Repeat("b", 48), State: report.StateComplete, ObservedAt: now,
		ExpiresAt: now.Add(30 * time.Minute), Terms: request.Terms,
	}}
	require.NoError(t, s.SaveTermReportHistory(t.Context(), item))
	page, err := s.ListTermReportHistory(t.Context(), 0, 1)
	require.NoError(t, err)
	require.Equal(t, item, page.Items[0])
	second := item
	second.Summary.ID = strings.Repeat("c", 48)
	second.Summary.ObservedAt = now.Add(time.Second)
	second.Summary.ExpiresAt = second.Summary.ObservedAt.Add(30 * time.Minute)
	require.NoError(t, s.SaveTermReportHistory(t.Context(), second))
	page, err = s.ListTermReportHistory(t.Context(), 0, 2)
	require.NoError(t, err)
	require.Equal(t, 2, page.Total)
	require.Equal(t, []TermReportHistory{second}, page.Items)
	page, err = s.ListTermReportHistory(t.Context(), 1, 2)
	require.NoError(t, err)
	require.Equal(t, []TermReportHistory{item}, page.Items)
}
