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
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

func TestReportHistoryMCPListsExactOwnerPageAndWithheldSummary(t *testing.T) {
	require.Contains(t, catalogNames(toolCatalog(false, false, false)), "list_report_history")
	id := strings.Repeat("a", 48)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only",
		Terms: []report.Term{{Number: 1, Expression: "synthetic alpha", Syntax: "simple",
			Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}}}}
	summary := report.Summary{ID: id, State: "visibility_changed", ObservedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), Terms: request.Terms}
	page := store.TermReportHistoryPage{Items: []store.TermReportHistory{{Request: request, Summary: summary}}, Total: 1}
	encodedPage, err := json.Marshal(page)
	require.NoError(t, err)
	var seenOffset, seenLimit string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-owner" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"title":"Forbidden","status":403,"code":"forbidden"}`))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/search-exports" {
			http.NotFound(w, r)
			return
		}
		seenOffset, seenLimit = r.URL.Query().Get("offset"), r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encodedPage)
	}))
	t.Cleanup(backend.Close)
	newLease := func(key string) *daemonLease {
		return newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
			return daemonconn.New(backend.URL, key), nil
		}, func(c *daemonconn.Connection) error { return c.Close() })
	}
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, newLease("synthetic-owner"))
	result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "list_report_history", "arguments": map[string]any{"offset": 0, "limit": 1},
	})))
	require.NotEqual(t, true, result["isError"])
	output := objectField(t, result, "structuredContent")
	total, ok := output["total"].(float64)
	require.True(t, ok)
	require.Equal(t, 1, int(total))
	items, ok := output["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok := items[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, id, objectField(t, item, "summary")["id"])
	require.Equal(t, "visibility_changed", objectField(t, item, "summary")["state"])
	require.NotContains(t, objectField(t, item, "summary"), "counts")
	terms, ok := objectField(t, item, "request")["terms"].([]any)
	require.True(t, ok)
	require.Len(t, terms, 1)
	term, ok := terms[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "synthetic alpha", term["expression"])
	require.Equal(t, "0", seenOffset)
	require.Equal(t, "1", seenLimit)

	foreign := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, newLease("foreign-owner"))
	denied := decodeResult(t, exchangeRaw(t, foreign, requestFor("tools/call", map[string]any{
		"name": "list_report_history", "arguments": map[string]any{"limit": 1},
	})))
	require.Equal(t, "access_denied", objectField(t, denied, "structuredContent")["code"])
}
