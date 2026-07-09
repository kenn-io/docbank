// internal/api/routes_read_test.go
package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestStatByIDAndPath(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	d, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)

	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d", d.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Equal(t, d.ID, n.ID)
	assert.Equal(t, "/docs", n.Path)
	assert.Equal(t, fmt.Sprintf("%q", strconv.FormatInt(d.Revision, 10)), resp.Header.Get("ETag"))

	resp, body = get(t, ts, "/api/v1/path?path=%2Fdocs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Equal(t, d.ID, n.ID)

	// Root stats fine; relative and missing paths are rejected.
	resp, _ = get(t, ts, "/api/v1/path?path=%2F", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp, body = get(t, ts, "/api/v1/path?path=docs", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)
	resp, body = get(t, ts, "/api/v1/path?path=%2Fnope", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, body, `"code":"not_found"`)
}

func TestChildrenPagination(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	for i := range 5 {
		_, err := s.Mkdir(ctx, s.RootID(), fmt.Sprintf("d%d", i))
		require.NoError(t, err)
	}
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/children?limit=2&offset=4", s.RootID()), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var page struct {
		Items []api.Node `json:"items"`
		Total int        `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 5, page.Total)
	assert.Len(t, page.Items, 1) // offset 4 of 5
}

func TestContentStreamsBlob(t *testing.T) {
	ts, s := newTestServer(t, nil)
	// Write a real blob through the test server's blob dir, then link it.
	n := createFileWithContent(t, ts, s, "/hello.txt", "hello world")
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/content", n.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "hello world", body)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
}

func TestContentOnDirIs422(t *testing.T) {
	ts, s := newTestServer(t, nil)
	d, err := s.Mkdir(t.Context(), s.RootID(), "d")
	require.NoError(t, err)
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/content", d.ID), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"not_file"`)
}

func TestSearch(t *testing.T) {
	ts, s := newTestServer(t, nil)
	_, err := s.CreateFile(t.Context(), s.RootID(), "insurance-2024.pdf", testHash("x"), 3, "application/pdf")
	require.NoError(t, err)
	resp, body := get(t, ts, "/api/v1/search?q=insurance", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "insurance-2024.pdf", body)
}
