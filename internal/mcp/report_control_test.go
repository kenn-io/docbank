package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

func TestReportControlReadsAreDefaultAndWritesRequireOwnOptIn(t *testing.T) {
	readOnly := catalogNames(toolCatalog(false, false, false))
	require.Contains(t, readOnly, "get_report_summary")
	require.Contains(t, readOnly, "get_report_dates")
	for _, name := range []string{"create_report", "revise_report"} {
		require.NotContains(t, readOnly, name)
	}
	writable := catalogNames(toolCatalog(false, false, true))
	for _, name := range []string{"create_report", "revise_report"} {
		require.Contains(t, writable, name)
	}
	require.NotContains(t, catalogNames(toolCatalog(false, true, false)), "create_report",
		"package write opt-in must not grant report writes")
	require.True(t, ServerOptions{AllowReportWrites: true}.AllowReportWrites)
}

func TestReportControlUsesTheOwnerBoundDaemonAndExactSelection(t *testing.T) {
	id := strings.Repeat("a", 48)
	childID := strings.Repeat("b", 48)
	member := report.Identity{NodeID: 7, VersionID: "synthetic-version", SHA256: strings.Repeat("c", 64)}
	request := report.Request{Version: 2, SelectedDocuments: []report.Identity{member}, Timezone: "UTC",
		CoverageMode: "strict", Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}
	created := report.Summary{ID: id, State: "needs_review", ObservedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), Terms: request.Terms, UnresolvedDates: 1}
	revised := created
	revised.ID, revised.ParentID = childID, id
	var seenCreate report.Request
	var seenChoices []report.DateChoice
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-owner" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"title":"Forbidden","status":403,"code":"forbidden"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/search-exports":
			if err := json.UnmarshalRead(r.Body, &seenCreate); err != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			_ = json.MarshalWrite(w, created)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/search-exports/"+id:
			_ = json.MarshalWrite(w, created)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/search-exports/"+id+"/dates":
			_ = json.MarshalWrite(w, report.DatePage{Members: []report.DateReviewMember{{Document: member,
				CandidatesComplete: true, Candidates: []report.DateCandidate{{ID: "candidate-1", Document: member,
					Locator: report.Locator{EvidenceSHA256: strings.Repeat("d", 64)}}}}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/search-exports/"+id+"/revisions":
			var body struct {
				Choices []report.DateChoice `json:"choices"`
			}
			if err := json.UnmarshalRead(r.Body, &body); err != nil {
				http.Error(w, "invalid choices", http.StatusBadRequest)
				return
			}
			seenChoices = body.Choices
			_ = json.MarshalWrite(w, revised)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(backend.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(backend.URL, "synthetic-owner"), nil
	}, func(client *daemonconn.Connection) error { return client.Close() })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowReportWrites: true}, lease)
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
			"name": name, "arguments": args,
		})))
		require.NotEqual(t, true, result["isError"], name)
		return objectField(t, result, "structuredContent")
	}
	createdOutput := call("create_report", map[string]any{"request": request})
	require.Equal(t, id, createdOutput["id"])
	require.Equal(t, request.SelectedDocuments, seenCreate.SelectedDocuments)
	require.Equal(t, id, call("get_report_summary", map[string]any{"report_id": id})["id"])
	page := call("get_report_dates", map[string]any{"report_id": id, "limit": 10})
	require.Len(t, page["members"], 1)
	choice := report.DateChoice{Document: member, CandidateID: "candidate-1", EvidenceSHA256: strings.Repeat("d", 64),
		Reason: "Synthetic reviewed date", Action: "select"}
	require.Equal(t, childID, call("revise_report", map[string]any{"report_id": id,
		"choices": []report.DateChoice{choice}})["id"])
	require.Equal(t, []report.DateChoice{choice}, seenChoices)
	foreign := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(backend.URL, "foreign-owner"), nil
	}, func(client *daemonconn.Connection) error { return client.Close() })
	foreignServer := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowReportWrites: true}, foreign)
	denied := decodeResult(t, exchangeRaw(t, foreignServer, requestFor("tools/call", map[string]any{
		"name": "get_report_summary", "arguments": map[string]any{"report_id": id},
	})))
	require.Equal(t, "access_denied", objectField(t, denied, "structuredContent")["code"])
}
