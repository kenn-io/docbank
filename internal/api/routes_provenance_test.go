package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
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

func TestAppendNodeProvenanceEndpointFencesAndReturnsReceipt(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node := createFileWithContent(t, ts, s, "/report.txt", "report")
	request := api.ProvenanceAppendRequest{
		SourceKind: "agent", SourceDescription: "triage", OriginalPath: "opaque://laptop/report",
	}
	resp, body := do(t, ts, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/provenance", node.ID),
		map[string]string{"If-Match": `"1"`}, request)
	assert.Equal(t, http.StatusCreated, resp.StatusCode, body)
	assert.Equal(t, `"2"`, resp.Header.Get("ETag"))
	var receipt api.ProvenanceAppendReceipt
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	assert.Equal(t, node.ID, receipt.Node.ID)
	assert.Equal(t, int64(2), receipt.Node.Revision)
	assert.Equal(t, "/report.txt", receipt.Path)
	assert.Equal(t, request.SourceKind, receipt.Fact.SourceKind)
	assert.Equal(t, request.OriginalPath, receipt.Fact.OriginalPath)

	resp, body = do(t, ts, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/provenance", node.ID), nil, request)
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"precondition_required"`)
	resp, body = do(t, ts, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/provenance", node.ID),
		map[string]string{"If-Match": `"1"`}, request)
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"stale_revision"`)
}

func TestAppendNodeProvenanceEndpointMapsProvenanceMismatch(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node := createFileWithContent(t, ts, s, "/report.txt", "report")
	missing := strings.Repeat("ef", 32)
	request := api.ProvenanceAppendRequest{
		SourceKind: "agent", SourceDescription: "triage",
		OriginalPath: "opaque://laptop/report", Supersedes: &missing,
	}
	resp, body := do(t, ts, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/provenance", node.ID),
		map[string]string{"If-Match": `"1"`}, request)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"provenance_mismatch"`)
}

func TestAppendNodeProvenanceEndpointRejectsInvalidOriginalMTime(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node := createFileWithContent(t, ts, s, "/report.txt", "report")
	for _, test := range []struct {
		value, code string
	}{
		{value: "not-a-time", code: "validation"},
		{value: "2026-08-26T14:00:00+02:00", code: "invalid_provenance_time"},
	} {
		request := api.ProvenanceAppendRequest{
			SourceKind: "agent", SourceDescription: "triage", OriginalPath: "opaque://report",
			OriginalMTime: &test.value,
		}
		resp, body := do(t, ts, http.MethodPost,
			fmt.Sprintf("/api/v1/nodes/%d/provenance", node.ID),
			map[string]string{"If-Match": `"1"`}, request)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.Contains(t, body, fmt.Sprintf(`"code":"%s"`, test.code))
	}
	current, err := s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.Revision, current.Revision)
}

func TestAppendNodeProvenanceEndpointRejectsMaintenanceAndBrowserSessions(t *testing.T) {
	gate := api.NewOperationGate()
	ts, s := newTestServer(t, func(d *api.Deps) { d.Gate = gate })
	node := createFileWithContent(t, ts, s, "/report.txt", "report")
	request := api.ProvenanceAppendRequest{
		SourceKind: "agent", SourceDescription: "triage", OriginalPath: "opaque://report",
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- gate.Maintain(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	resp, body := do(t, ts, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/provenance", node.ID),
		map[string]string{"If-Match": `"1"`}, request)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"maintenance_busy"`)
	close(release)
	require.NoError(t, <-done)

	webSession, webBody := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, webSession.StatusCode, webBody)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(webBody), &issued))
	resp, body = do(t, ts, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/provenance", node.ID),
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}, request)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"web_session_read_only"`)
}

func TestAppendNodeProvenanceEndpointRejectsOversizedRequestBody(t *testing.T) {
	ts, s := newTestServer(t, nil)
	node := createFileWithContent(t, ts, s, "/report.txt", "report")
	request := map[string]any{
		"source_kind":        "agent",
		"source_description": "triage",
		"original_path":      "opaque://report",
		"padding":            strings.Repeat("x", 2<<20),
	}
	resp, body := do(t, ts, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/provenance", node.ID),
		map[string]string{"If-Match": `"1"`}, request)
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, body)
	current, err := s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.Revision, current.Revision)
}
