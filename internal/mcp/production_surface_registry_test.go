package mcp

import (
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/require"

	production "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
)

// The public surface catalog must describe operations the daemon actually
// serves. Approval issuance stays with an authenticated embedded host.
func TestProductionSurfaceRegistryMatchesLiveRoutesAndTools(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	actual := make(map[string]string)
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, "/api/v1/productions/") &&
			!strings.HasPrefix(path, "/api/v1/production-") {
			continue
		}
		for method, operation := range map[string]*huma.Operation{
			"GET": item.Get, "POST": item.Post, "PUT": item.Put,
			"PATCH": item.Patch, "DELETE": item.Delete,
		} {
			if operation != nil {
				actual[operation.OperationID] = method + " " + path
			}
		}
	}

	advertised := make(map[string]string)
	tools := catalogMap(toolCatalog(true))
	for _, operation := range production.SurfaceOperations() {
		advertised[operation.OperationID] = operation.Method + " " + operation.Path
		if operation.Tool != "" {
			require.Contains(t, tools, operation.Tool, operation.OperationID)
		}
	}
	require.Equal(t, actual, advertised)
	require.NotContains(t, advertised, "recordProductionApproval")
	require.NotContains(t, advertised, "revokeProductionApproval")
	require.NotContains(t, advertised, "supersedeProductionApproval")
}
