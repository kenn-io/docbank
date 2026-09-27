package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
)

func TestProductionApprovalReadToolReturnsPublicProjection(t *testing.T) {
	const approvalID = "11111111-1111-4111-8111-111111111111"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/production-approvals/"+approvalID, r.URL.Path)
		writeDaemonJSON(t, w, api.ProductionApprovalPublic{
			Grant: documentproduction.ApprovalPublicGrant{
				ID: approvalID, SubjectSHA256: strings.Repeat("1", 64), Actor: "synthetic-reviewer",
				GrantedAt: "2026-09-22T14:00:00Z", SHA256: strings.Repeat("2", 64),
			},
			Events: []documentproduction.ApprovalPublicEvent{{
				ID: "33333333-3333-4333-8333-333333333333", ApprovalID: approvalID,
				Kind: documentproduction.ApprovalEventRevoke, EffectiveAt: "2026-09-22T14:01:00Z",
			}},
		})
	}))
	t.Cleanup(daemon.Close)
	server := newBatesToolTestServer(t, daemon.URL, false)
	require.NotNil(t, catalogMap(toolCatalog(false))["get_production_approval"])
	result := callToolResult(t, server, "get_production_approval", map[string]any{"approval_id": approvalID})
	public := objectField(t, result, "structuredContent")
	require.Equal(t, "synthetic-reviewer", objectField(t, public, "grant")["actor"])
	require.Len(t, public["events"], 1)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "evidence")
	require.NotContains(t, string(encoded), "reason")
	tool := catalogMap(toolCatalog(false))["get_production_approval"]
	assertSchemaAccepts(t, tool.InputSchema, map[string]any{"approval_id": approvalID})
	assertSchemaRejects(t, tool.InputSchema, map[string]any{"approval_id": "not-a-uuid"})
}
