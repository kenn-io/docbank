package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionMCPReviewPagesAndJobStatusUseDaemon(t *testing.T) {
	const setID = "79000000-0000-4000-8000-000000000091"
	const memberID = "79000000-0000-4000-8000-000000000092"
	const sourceID = "79000000-0000-4000-8000-000000000093"
	const jobID = "79000000-0000-4000-8000-000000000094"
	digest := strings.Repeat("a", 64)
	member := redaction.Member{ID: memberID, VaultID: "79000000-0000-4000-8000-000000000095",
		SourceVersionID: sourceID, SourceSHA256: digest, PDFSHA256: digest, NodeID: 1,
		SourceSize: 10, PDFSize: 10, Ordinal: 1,
		Family: redaction.FamilyContext{Kind: "standalone", RootVersionID: sourceID},
		Mode:   "keep_selected", MapSHA256: digest, PageInventorySHA256: digest}
	decision := redaction.Decision{ID: "79000000-0000-4000-8000-000000000096", MemberID: memberID,
		Action: "keep", Reason: "synthetic reason", Label: "synthetic", Uncertain: true, Revision: 1,
		Actor: "synthetic-operator", CreatedAt: "2026-09-25T00:00:00Z",
		Selector: redaction.Selector{Kind: "text", MapSHA256: digest, Span: &redaction.Span{Start: 0, End: 1}}}
	status := api.ProductionJobStatus{JobID: jobID, SetID: setID, Revision: 1,
		State: "queued", RevisionSHA256: digest}
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		assert.Equal(t, http.MethodGet, request.Method)
		switch request.URL.Path {
		case "/api/v1/productions/sets/" + setID + "/revisions/1/members":
			assert.Equal(t, "1", request.URL.Query().Get("limit"))
			writeDaemonJSON(t, response, api.ProductionMemberPage{Items: []api.ProductionMember{api.ProductionMember(member)}})
		case "/api/v1/productions/sets/" + setID + "/revisions/1/decisions":
			assert.Equal(t, "1", request.URL.Query().Get("limit"))
			assert.Equal(t, "true", request.URL.Query().Get("uncertain"))
			writeDaemonJSON(t, response, api.ProductionDecisionPage{Items: []api.ProductionDecision{api.ProductionDecision(decision)}})
		case "/api/v1/productions/sets/" + setID + "/jobs/" + jobID:
			writeDaemonJSON(t, response, status)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, false)
	members := objectField(t, callToolResult(t, server, "list_production_members",
		map[string]any{"set_id": setID, "revision": 1, "limit": 1}), "structuredContent")
	require.Len(t, members["items"], 1)
	memberItems, ok := members["items"].([]any)
	require.True(t, ok)
	memberSummary, ok := memberItems[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, memberID, memberSummary["id"])
	assert.Equal(t, "private", members["cacheScope"])
	decisions := objectField(t, callToolResult(t, server, "list_production_decisions",
		map[string]any{"set_id": setID, "revision": 1, "limit": 1, "uncertain": "true"}), "structuredContent")
	require.Len(t, decisions["items"], 1)
	decisionItems, ok := decisions["items"].([]any)
	require.True(t, ok)
	decisionSummary, ok := decisionItems[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "keep", decisionSummary["action"])
	job := objectField(t, callToolResult(t, server, "get_production_job",
		map[string]any{"set_id": setID, "job_id": jobID}), "structuredContent")
	assert.Equal(t, "queued", objectField(t, job, "job")["state"])
	assert.Equal(t, 3, calls)
}

func TestProductionMCPReviewReadSchemasBoundPages(t *testing.T) {
	tools := catalogMap(toolCatalog(false))
	members := tools["list_production_members"]
	require.NotNil(t, members)
	assertSchemaRejects(t, members.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-000000000091", "revision": 1, "limit": 201})
	decisions := tools["list_production_decisions"]
	require.NotNil(t, decisions)
	assertSchemaRejects(t, decisions.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-000000000091", "revision": 1, "limit": 101})
	assertSchemaRejects(t, decisions.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-000000000091", "revision": 1, "uncertain": "maybe"})
	job := tools["get_production_job"]
	require.NotNil(t, job)
	assertSchemaRejects(t, job.InputSchema, map[string]any{
		"set_id": "79000000-0000-4000-8000-000000000091", "job_id": "bad"})
}
