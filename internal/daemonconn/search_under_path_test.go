package daemonconn_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestSearchUnderPathTransport(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/Café", r.URL.Query().Get("under_path"))
		require.NotContains(t, r.URL.Query(), "under_node_id")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/search/text-coverage" {
			require.NoError(t, json.MarshalWrite(w, api.SearchTextCoverage{NoText: 1}))
			return
		}
		require.Equal(t, "/api/v1/search", r.URL.Path)
		require.NoError(t, json.MarshalWrite(w, api.SearchReport{Hits: []api.SearchHit{}, Limit: 10,
			UnderPath: "/Café", UnderNodeID: 42}))
	}))
	t.Cleanup(ts.Close)
	c := daemonconn.New(ts.URL, "synthetic-key")
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	byPath, err := c.SearchWithOptions(t.Context(), "mercury", 10, daemonconn.SearchOptions{UnderPath: "/Café"})
	require.NoError(t, err)
	require.Equal(t, "/Café", byPath.UnderPath)
	require.EqualValues(t, 42, byPath.UnderNodeID)
	coveragePath, err := c.SearchTextCoverage(t.Context(), daemonconn.SearchOptions{UnderPath: "/Café"})
	require.NoError(t, err)
	require.Equal(t, 1, coveragePath.NoText)
}

func TestSearchUnderPathValidationBeforeTransport(t *testing.T) {
	var calls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	c := daemonconn.New(ts.URL, "synthetic-key")
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	scope := daemonconn.SearchOptions{UnderPath: "/" + strings.Repeat("é", 8192)}
	_, err := c.SearchWithOptions(t.Context(), "mercury", 10, scope)
	require.ErrorIs(t, err, store.ErrInvalidSearchScope)
	_, err = c.SearchTextCoverage(t.Context(), scope)
	require.ErrorIs(t, err, store.ErrInvalidSearchScope)
	_, err = c.ResolveDocumentSourceFence(t.Context(), api.DocumentSourceFenceResolveRequest{
		Filters: &api.DocumentSourceFenceFilters{UnderPath: scope.UnderPath},
	})
	require.ErrorIs(t, err, store.ErrInvalidSearchScope)
	require.Zero(t, calls.Load())
}

func TestSearchUnderPathReceiptAuthority(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options daemonconn.SearchOptions
		path    string
		id      int64
		valid   bool
	}{
		{"resolved directory", daemonconn.SearchOptions{UnderPath: "/notes"}, "/notes", 42, true},
		{"missing path", daemonconn.SearchOptions{UnderPath: "/notes"}, "", 42, false},
		{"missing ID", daemonconn.SearchOptions{UnderPath: "/notes"}, "/notes", 0, false},
		{"negative ID", daemonconn.SearchOptions{UnderPath: "/notes"}, "/notes", -1, false},
		{"different path", daemonconn.SearchOptions{UnderPath: "/notes"}, "/other", 42, false},
		{"different ID", daemonconn.SearchOptions{UnderNodeID: 42}, "", 43, false},
		{"unexpected ID", daemonconn.SearchOptions{}, "", 42, false},
		{"unexpected path", daemonconn.SearchOptions{}, "/notes", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, tc.options.UnderPath, r.URL.Query().Get("under_path"))
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.MarshalWrite(w, api.SearchReport{Hits: []api.SearchHit{}, Limit: 10,
					UnderPath: tc.path, UnderNodeID: tc.id}))
			}))
			t.Cleanup(ts.Close)
			c := daemonconn.New(ts.URL, "synthetic-key")
			t.Cleanup(func() { require.NoError(t, c.Close()) })
			_, err := c.SearchWithOptions(t.Context(), "mercury", 10, tc.options)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "inconsistent filter authority")
			}
		})
	}
}
