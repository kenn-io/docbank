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
