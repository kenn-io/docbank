package agentops

import (
	"slices"
	"testing"
)

func TestNativeReportAndExportOperationsBindExistingCLIMCPAdapters(t *testing.T) {
	operations := CurrentOperations()
	for _, check := range []struct {
		id, route, cli string
		tools          []string
	}{
		{"report_create", "POST /api/v1/search-exports", "search-export create", []string{"create_report"}},
		{"report_history", "GET /api/v1/search-exports", "search-export history", []string{"list_report_history"}},
		{"report_summary", "GET /api/v1/search-exports/{id}", "search-export show", []string{"get_report_summary"}},
		{"report_date_review", "POST /api/v1/search-exports/{id}/dates", "search-export dates", []string{"get_report_dates"}},
		{"report_revise", "POST /api/v1/search-exports/{id}/revisions", "search-export revise", []string{"revise_report"}},
		{"report_artifact_download", "GET /api/v1/search-exports/{id}/csv", "search-export download", []string{"open_report_artifact", "download_report_artifact"}},
		{"export_source_create", "POST /api/v1/exports/sources", "export source", []string{"create_export_source"}},
		{"export_plan_create", "POST /api/v1/exports/plans", "export plan", []string{"create_export_plan"}},
		{"export_plan_preview", "GET /api/v1/exports/plans/{id}/preview", "export preview", []string{"preview_export_plan"}},
		{"export_plan_read", "GET /api/v1/exports/plans/{id}", "export show-plan", []string{"get_export_plan"}},
		{"export_output_problems", "GET /api/v1/exports/plans/{id}/problems", "export problems", []string{"list_export_output_problems"}},
		{"export_job_start", "POST /api/v1/exports/jobs", "export start", []string{"start_export_job"}},
		{"export_job_status", "GET /api/v1/exports/jobs/{id}", "export status", []string{"get_export_job"}},
		{"export_job_cancel", "POST /api/v1/exports/jobs/{id}/cancel", "export cancel", []string{"cancel_export_job"}},
		{"export_archive_download", "GET /api/v1/exports/jobs/{id}/archive", "export archive", []string{"open_export_archive", "download_export_archive"}},
	} {
		t.Run(check.id, func(t *testing.T) {
			operation := operationByID(t, operations, check.id)
			if !slices.Contains(operation.RouteIDs, check.route) || !slices.Contains(operation.CLIPaths, check.cli) ||
				!slices.Equal(operation.MCPTools, check.tools) || len(operation.SurfaceGaps) != 0 {
				t.Fatalf("native adapter binding is incomplete: %+v", operation)
			}
			if !operation.ReviewedBehavior() {
				t.Fatalf("native operation lacks reviewed bounds and wire references: %+v", operation)
			}
		})
	}
}

func TestNativeDocumentOperationsPublishReviewedCLIMCPContracts(t *testing.T) {
	for _, check := range []struct {
		id, feature, input, output, paging string
		defaultPage, maxPage               int
		idempotent                         bool
	}{
		{"list_documents", "native_documents", "api.DocumentQuery|api.ScopedDocumentQuery", "api.DocumentPage", "cursor", 50, 250, true},
		{"search_documents", "native_documents", "api.DocumentSourceFenceResolveRequest|api.DocumentSearchRequest", "api.DocumentSearchReport", "none", 0, 0, true},
		{"get_document", "native_documents", "GetNodePath", "api.Node", "none", 0, 0, true},
		{"list_document_versions", "native_documents", "ListContentVersionsQuery", "api.ContentVersionPage", "offset", 100, 250, true},
		{"read_rendition_text", "native_documents", "api.RenditionWindowRequest", "api.RenditionTextWindow", "none", 0, 0, true},
		{"get_processing_plan", "native_processing", "api.ProcessingPlanRequest", "api.ProcessingPlan", "none", 0, 0, true},
		{"get_processing_status", "native_processing", "GetDocumentProcessingJobPath", "api.ProcessingStatus", "none", 0, 0, true},
		{"get_processing_coverage", "native_processing", "GetDocumentProcessingCoverageQuery", "api.CoverageReport", "none", 0, 0, true},
		{"list_processing_profiles", "native_processing", "empty object", "[]api.ProcessingProfileSummary", "none", 0, 0, true},
		{"start_processing", "native_processing", "api.StartProcessingRequest", "api.ProcessingJobEvent", "none", 0, 0, false},
		{"format_coverage", "native_processing", "ReadFormatCapabilitiesQuery", "api.FormatCoverageResponse", "none", 0, 0, true},
	} {
		t.Run(check.id, func(t *testing.T) {
			operation := operationByID(t, CurrentOperations(), check.id)
			if len(operation.CLIPaths) == 0 || len(operation.MCPTools) == 0 ||
				len(operation.SurfaceGaps) != 0 || !operation.ReviewedBehavior() ||
				operation.Feature != check.feature || operation.InputRef != check.input ||
				operation.OutputRef != check.output || operation.Bounds.Paging != check.paging ||
				operation.Bounds.Default != check.defaultPage || operation.Bounds.Maximum != check.maxPage ||
				operation.Bounds.ResponseBytes != 1<<20 || operation.Idempotent != check.idempotent ||
				operation.Destructive {
				t.Fatalf("native document parity contract is incomplete: %+v", operation)
			}
		})
	}
}

func TestNativeProcessingProfilesHaveBothAgentSurfaces(t *testing.T) {
	operation := operationByID(t, CurrentOperations(), "list_processing_profiles")
	if !slices.Equal(operation.RouteIDs, []string{"GET /api/v1/processing/profiles"}) ||
		!slices.Equal(operation.CLIPaths, []string{"processing profiles"}) ||
		!slices.Equal(operation.MCPTools, []string{"list_processing_profiles"}) ||
		!operation.ReviewedBehavior() || len(operation.SurfaceGaps) != 0 {
		t.Fatalf("processing profiles are not available as one reviewed operation: %+v", operation)
	}
}

func TestNativeTagListHasReviewedCLIMCPBinding(t *testing.T) {
	operation := operationByID(t, CurrentOperations(), "list_tags")
	if !slices.Equal(operation.RouteIDs, []string{"GET /api/v1/tags"}) ||
		!slices.Equal(operation.CLIPaths, []string{"tag list"}) ||
		!slices.Equal(operation.MCPTools, []string{"list_tags"}) ||
		operation.Bounds.Paging != "offset" || operation.Bounds.Maximum != 250 ||
		!operation.ReviewedBehavior() || len(operation.SurfaceGaps) != 0 {
		t.Fatalf("tag listing lacks reviewed CLI/MCP parity: %+v", operation)
	}
}
