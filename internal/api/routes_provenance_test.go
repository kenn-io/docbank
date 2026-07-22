package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestNodeProvenanceEndpointReturnsOriginAuthority(t *testing.T) {
	ts, s := newTestServer(t, nil)
	run, err := s.BeginIngest(t.Context(), "watch", "agent-sessions")
	require.NoError(t, err)
	node, added, err := s.IngestFile(t.Context(), run, s.RootID(), "session.jsonl",
		testHash("session"), 7, "application/x-ndjson", "closed/session.jsonl",
		"2026-07-21T13:14:15Z")
	require.NoError(t, err)
	require.True(t, added)

	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/provenance?limit=1&offset=0", node.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.ProvenancePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, node.ID, page.Node.ID)
	assert.Equal(t, "/session.jsonl", page.Node.Path)
	assert.Equal(t, 1, page.Total)
	assert.Equal(t, 1, page.Limit)
	require.Len(t, page.Items, 1)
	fact := page.Items[0]
	assert.Equal(t, node.ID, fact.NodeID)
	assert.Equal(t, run.ID(), fact.IngestID)
	assert.Equal(t, "watch", fact.SourceKind)
	assert.Equal(t, "agent-sessions", fact.SourceDescription)
	assert.Equal(t, "closed/session.jsonl", fact.OriginalPath)
	require.NotNil(t, fact.OriginalMTime)
	assert.Equal(t, "2026-07-21T13:14:15Z", *fact.OriginalMTime)
	assert.True(t, fact.Active)
	assert.Nil(t, fact.Supersedes)
}

func TestNodeProvenanceEndpointMapsInvalidTargets(t *testing.T) {
	ts, s := newTestServer(t, nil)
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/provenance", s.RootID()), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"not_file"`)

	resp, body = get(t, ts, "/api/v1/nodes/99999/provenance", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"not_found"`)

	resp, body = get(t, ts, "/api/v1/nodes/0/provenance", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"validation"`)

	_, err := s.NodeProvenance(t.Context(), s.RootID(), 10, 0)
	require.ErrorIs(t, err, store.ErrNotFile)
}
