package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestListTagsReadsBoundedDaemonPage(t *testing.T) {
	const tagID = "33333333-3333-4333-8333-333333333333"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/tags", r.URL.Path)
		assert.Equal(t, "25", r.URL.Query().Get("limit"))
		assert.Equal(t, "0", r.URL.Query().Get(tagOffsetField))
		writeDaemonJSON(t, w, api.TagPage{Items: []api.Tag{{ID: tagID, Name: "Review", Revision: 1, AssignmentCount: 2}}, Total: 1, Limit: 25})
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	tool := catalogMap(toolCatalog(false, false, false))["list_tags"]
	require.NotNil(t, tool)
	result, err := invokeReadTool(t.Context(), lease, "list_tags", map[string]any{"limit": 25, "offset": 0})
	require.NoError(t, err)
	output := structuredMap(t, result.StructuredContent)
	assertSchemaAccepts(t, tool.OutputSchema, output)
	assert.Equal(t, "private", output["cacheScope"])
	assert.EqualValues(t, 1, output["total"])
	items, ok := output["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	item, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, tagID, item["id"])
}

func TestListTagsRejectsOutOfBoundsPageBeforeDaemonCall(t *testing.T) {
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		t.Fatal("invalid page must not connect to daemon")
		return nil, errors.New("unexpected daemon connection")
	}, nil)
	for _, args := range []map[string]any{{"limit": 0}, {"limit": 1001}, {tagOffsetField: -1}} {
		_, err := invokeReadTool(t.Context(), lease, "list_tags", args)
		require.Error(t, err)
	}
}

func TestListTagsEmptyPageUsesDefaultAndKeepsArrayShape(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "100", r.URL.Query().Get("limit"))
		assert.Equal(t, "0", r.URL.Query().Get(tagOffsetField))
		writeDaemonJSON(t, w, api.TagPage{Total: 0, Limit: 100})
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	result, err := invokeReadTool(t.Context(), lease, "list_tags", map[string]any{})
	require.NoError(t, err)
	items, ok := structuredMap(t, result.StructuredContent)["items"].([]any)
	require.True(t, ok)
	assert.Empty(t, items)
}
