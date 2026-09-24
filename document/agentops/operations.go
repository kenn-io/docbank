package agentops

import "strings"

// CurrentOperations names domain operations with real bindings on this base.
// An operation may use several HTTP routes. CurrentRoutes is the independent,
// exhaustive security census; absence from this list never grants a route.
func CurrentOperations() []Operation {
	operations := []Operation{
		currentOperation("vault_info", []string{"GET /api/v1/info"}, "info", "get_vault_info", true),
		currentOperation("list_documents", []string{"GET /api/v1/documents", "POST /api/v1/documents/scoped"}, "documents list", "list_documents", false),
		currentOperation("search_documents", []string{
			"POST /api/v1/processing/source-fences/resolve", "POST /api/v1/search/validate",
			"POST /api/v1/search", "POST /api/v1/documents/resolve",
		}, "search", "search_documents", false),
		currentOperation("get_document", []string{"GET /api/v1/nodes/{id}"}, "stat", "get_document", false),
		currentOperation("list_document_versions", []string{"GET /api/v1/nodes/{id}/versions"}, "versions list", "list_document_versions", false),
		currentOperation("read_rendition_text", []string{"POST /api/v1/renditions/windows"}, "rendition text", "read_rendition_text", false),
		currentOperation("get_processing_plan", []string{"POST /api/v1/processing/plans"}, "processing plan", "get_processing_plan", false),
		currentOperation("get_processing_status", []string{"GET /api/v1/processing/jobs/{id}"}, "processing status", "get_processing_status", false),
		currentOperation("get_processing_coverage", []string{"GET /api/v1/coverage"}, "processing coverage", "get_processing_coverage", false),
		currentOperation("start_processing", []string{"POST /api/v1/processing/jobs"}, "processing build", "start_processing", false),
		currentOperation("format_coverage", []string{"GET /api/v1/formats/capabilities"}, "formats", "get_format_coverage", false),
		currentOperation("report_create", []string{"POST /api/v1/search-exports"}, "search-export create", "create_report", false),
		currentOperation("report_history", []string{"GET /api/v1/search-exports"}, "search-export history", "list_report_history", false),
		currentOperation("report_summary", []string{"GET /api/v1/search-exports/{id}"}, "search-export show", "get_report_summary", false),
		currentOperation("report_date_review", []string{"POST /api/v1/search-exports/{id}/dates"}, "search-export dates", "get_report_dates", false),
		currentOperation("report_revise", []string{"POST /api/v1/search-exports/{id}/revisions"}, "search-export revise", "revise_report", false),
		currentOperation("report_artifact_download", []string{
			"GET /api/v1/search-exports/{id}/csv", "GET /api/v1/search-exports/{id}/bundle",
		}, "search-export download", "open_report_artifact", false),
		currentOperation("browser_report_ticket", []string{"POST /api/v1/search-exports/{id}/download"}, "", "", true),
		currentOperation("browser_export_ticket", []string{"POST /api/v1/exports/jobs/{id}/download"}, "", "", true),
		currentOperation("export_source_create", []string{"POST /api/v1/exports/sources"}, "export source", "create_export_source", false),
		currentOperation("export_plan_create", []string{"POST /api/v1/exports/plans"}, "export plan", "create_export_plan", false),
		currentOperation("export_plan_preview", []string{"GET /api/v1/exports/plans/{id}/preview"}, "export preview", "preview_export_plan", false),
		currentOperation("export_job_start", []string{"POST /api/v1/exports/jobs"}, "export start", "start_export_job", false),
		currentOperation("export_job_status", []string{"GET /api/v1/exports/jobs/{id}"}, "export status", "get_export_job", false),
		currentOperation("export_job_cancel", []string{"POST /api/v1/exports/jobs/{id}/cancel"}, "export cancel", "cancel_export_job", false),
		currentOperation("export_archive_download", []string{
			"GET /api/v1/exports/jobs/{id}/archive",
			"GET /api/v1/exports/jobs/{id}/archive/authority",
		}, "export archive", "open_export_archive", false),
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
			operations[i].MCPTools = append(operations[i].MCPTools, "download_report_artifact")
		case "export_job_start":
			operations[i].Job = JobSemantics{StatusRouteID: "GET /api/v1/exports/jobs/{id}",
				CancelRouteID: "POST /api/v1/exports/jobs/{id}/cancel", Durable: true}
			operations[i].Receipt = ReceiptSemantics{Kind: "export-job", Replay: "operation-id"}
		case "export_archive_download":
			operations[i].MCPTools = append(operations[i].MCPTools, "download_export_archive")
			operations[i].Receipt = ReceiptSemantics{Kind: "verified-export-archive", Replay: "same-retained-bytes"}
		}
		reviewNativeReportExportOperation(&operations[i])
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

func reviewNativeReportExportOperation(operation *Operation) {
	if operation == nil {
		return
	}
	var input, output string
	bound := Bounds{Paging: "none", ResponseBytes: 16 << 20}
	feature := "native_reports"
	idempotent, destructive := false, false
	switch operation.ID {
	case "report_create":
		input, output = "report.Request", "report.Summary"
	case "report_history":
		input, output = "ListTermReportHistoryQuery", "store.TermReportHistoryPage"
		bound = Bounds{Paging: "offset", Default: 20, Maximum: 50, ResponseBytes: 16 << 20}
		idempotent = true
	case "report_summary":
		input, output = "GetTermReportPath", "report.Summary"
		idempotent = true
	case "report_date_review":
		input, output = "report.DatePageRequest", "report.DatePage"
		bound = Bounds{Paging: "cursor", Default: 50, Maximum: 100, ResponseBytes: 16 << 20}
		idempotent = true
	case "report_revise":
		input, output = "report.DateChoice", "report.Summary"
	case "report_artifact_download":
		input, output = "DownloadTermReportcsvPath|DownloadTermReportbundlePath", "daemonconn.TermReportStream"
		bound.ResponseBytes = 512 << 20
	case "export_source_create":
		input, output = "bundle.SourceRequest", "bundle.Source"
		feature, idempotent = "native_exports", true
	case "export_plan_create":
		input, output = "bundle.PlanRequest", "bundle.Plan"
		feature, idempotent = "native_exports", true
	case "export_plan_preview":
		input, output = "GetExportPlanPreviewPath", "bundle.PlanPreview"
		feature, idempotent = "native_exports", true
	case "export_job_start":
		input, output = "bundle.JobRequest", "bundle.ExportJob"
		feature, idempotent = "native_exports", true
	case "export_job_status":
		input, output = "GetExportJobPath", "bundle.ExportJob"
		feature, idempotent = "native_exports", true
	case "export_job_cancel":
		input, output = "CancelExportJobPath", "empty 204 response"
		feature, idempotent, destructive = "native_exports", true, true
	case "export_archive_download":
		input, output = "ReadExportArchivePath", "daemonconn.ExportArchiveStream"
		feature = "native_exports"
		bound.ResponseBytes = 512 << 20
	default:
		return
	}
	operation.Feature, operation.Qualification = feature, "reviewed"
	operation.Bounds, operation.InputRef, operation.OutputRef = bound, input, output
	operation.Idempotent, operation.Destructive = idempotent, destructive
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
