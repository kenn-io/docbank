package agentops

import "strings"

// CurrentOperations names domain operations with real bindings on this base.
// An operation may use several HTTP routes. CurrentRoutes is the independent,
// exhaustive security census; absence from this list never grants a route.
func CurrentOperations() []Operation {
	operations := []Operation{
		currentOperation("vault_info", []string{"GET /api/v1/info"}, "info", "get_vault_info", true),
		currentOperation("list_documents", []string{"GET /api/v1/documents"}, "", "list_documents", false),
		currentOperation("search_documents", []string{
			"POST /api/v1/processing/source-fences/resolve", "POST /api/v1/search/validate",
			"POST /api/v1/search", "POST /api/v1/documents/resolve",
		}, "search", "search_documents", false),
		currentOperation("get_document", []string{"GET /api/v1/nodes/{id}"}, "stat", "get_document", false),
		currentOperation("list_document_versions", []string{"GET /api/v1/nodes/{id}/versions"}, "versions list", "list_document_versions", false),
		currentOperation("read_rendition_text", []string{"POST /api/v1/renditions/windows"}, "", "read_rendition_text", false),
		currentOperation("get_processing_plan", []string{"POST /api/v1/processing/plans"}, "processing plan", "get_processing_plan", false),
		currentOperation("get_processing_status", []string{"GET /api/v1/processing/jobs/{id}"}, "processing status", "get_processing_status", false),
		currentOperation("get_processing_coverage", []string{"GET /api/v1/coverage"}, "", "get_processing_coverage", false),
		currentOperation("start_processing", []string{"POST /api/v1/processing/jobs"}, "processing build", "start_processing", false),
		currentOperation("format_coverage", []string{"GET /api/v1/formats/capabilities"}, "formats", "", false),
		currentOperation("report_create", []string{"POST /api/v1/search-exports"}, "search-export create", "", false),
		currentOperation("report_date_review", []string{"POST /api/v1/search-exports/{id}/dates"}, "search-export dates", "", false),
		currentOperation("report_revise", []string{"POST /api/v1/search-exports/{id}/revisions"}, "search-export revise", "", false),
		currentOperation("report_artifact_download", []string{
			"GET /api/v1/search-exports/{id}/csv", "GET /api/v1/search-exports/{id}/bundle",
		}, "", "", false),
		currentOperation("browser_report_ticket", []string{"POST /api/v1/search-exports/{id}/download"}, "", "", true),
		currentOperation("browser_export_ticket", []string{"POST /api/v1/exports/jobs/{id}/download"}, "", "", true),
		currentOperation("package_preflight", []string{"POST /api/v1/packages/preflights"}, "package preflight", "", true),
		currentOperation("prune_content_versions", []string{"POST /api/v1/nodes/{id}/versions/prune"}, "", "", true),
	}
	for i := range operations {
		switch operations[i].ID {
		case "start_processing":
			operations[i].Job = JobSemantics{StatusRouteID: "GET /api/v1/processing/jobs/{id}", Durable: true}
			operations[i].Receipt = ReceiptSemantics{Kind: "processing-job", Replay: "operation-id"}
		case "report_create":
			operations[i].Receipt = ReceiptSemantics{Kind: "frozen-report-summary", Replay: "new-observation"}
		case "report_artifact_download":
			operations[i].Receipt = ReceiptSemantics{Kind: "verified-artifact", Replay: "same-frozen-bytes"}
			for j := range operations[i].SurfaceGaps {
				switch operations[i].SurfaceGaps[j].Surface {
				case "cli":
					operations[i].SurfaceGaps[j].Reason = "No standalone CLI download for an existing frozen report ID"
				case "mcp":
					operations[i].SurfaceGaps[j].Reason = "No owner-bound typed MCP handle for the verified artifact"
				}
			}
		}
	}
	covered := make(map[string]bool)
	for _, operation := range operations {
		for _, id := range operation.RouteIDs {
			covered[id] = true
		}
	}
	for _, route := range CurrentRoutes() {
		if route.OperatorOnly || covered[route.ID] {
			continue
		}
		operation := currentOperation(unqualifiedOperationID(route), []string{route.ID}, "", "", false)
		operation.Feature = "unqualified"
		operation.SurfaceGaps = append(operation.SurfaceGaps, SurfaceGap{
			Surface: "contract", Owner: "agentops",
			Reason: "Domain identity, bounds, job and receipt semantics require review",
		})
		operations = append(operations, operation)
	}
	return operations
}

func unqualifiedOperationID(route Route) string {
	path := strings.TrimPrefix(route.Pattern, "/api/v1/")
	path = strings.NewReplacer("/", "_", "-", "_", "{", "by_", "}", "").Replace(path)
	return "unqualified_" + strings.ToLower(route.Method) + "_" + path
}

func currentOperation(id string, routeIDs []string, cli, mcp string, operatorOnly bool) Operation {
	operation := Operation{ID: id, RouteIDs: routeIDs, OperatorOnly: operatorOnly}
	if cli != "" {
		operation.CLIPaths = []string{cli}
	} else if !operatorOnly {
		operation.SurfaceGaps = append(operation.SurfaceGaps, SurfaceGap{
			Surface: "cli", Owner: "agentops", Reason: "No equivalent CLI operation",
		})
	}
	if mcp != "" {
		operation.MCPTools = []string{mcp}
	} else if !operatorOnly {
		operation.SurfaceGaps = append(operation.SurfaceGaps, SurfaceGap{
			Surface: "mcp", Owner: "agentops", Reason: "No typed MCP operation",
		})
	}
	return operation
}
