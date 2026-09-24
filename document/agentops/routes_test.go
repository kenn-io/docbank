package agentops

import (
	"slices"
	"testing"
)

func TestAgentRouteEffectsFollowHandlerBehavior(t *testing.T) {
	for _, test := range []struct {
		pattern      string
		class        Class
		operatorOnly bool
	}{
		{"POST /api/v1/search-exports/{id}/dates", Read, false},
		{"POST /api/v1/pages/jobs/{id}", Read, false},
		{"POST /api/v1/mailbox/containers/{id}/preview", Read, false},
		{"POST /api/v1/nodes/{id}/verify", Read, false},
		{"POST /api/v1/pages/jobs", Write, false},
		{"POST /api/v1/mailbox/jobs", Write, false},
		{"POST /api/v1/search-exports", Write, false},
		{"POST /api/v1/search-exports/{id}/download", Session, true},
		{"POST /api/v1/exports/jobs/{id}/download", Session, true},
		{"GET /api/v1/exports/plans/{id}/problems", Read, false},
		{"GET /api/v1/exports/sources/{id}/attachment-publications", Read, false},
		{"GET /api/v1/exports/sources/{id}/email-pdf-recipes", Read, false},
		{"GET /api/v1/packages/by-id/{package_id}/timeline-inputs", Read, false},
		{"GET /api/v1/packages/field-catalog", Read, false},
		{"POST /api/v1/packages/containers/{id}/preflight", Write, false},
		{"POST /api/v1/packages/custodians/{assignment_id}/resolve", Write, false},
		{"PUT /api/v1/packages/by-id/{package_id}/custodian", Write, false},
		{"POST /api/v1/packages/preflights", Write, true},
		{"POST /api/v1/nodes/{id}/versions/prune", Admin, true},
		{"PUT /api/v1/nodes/{id}/content", Write, false},
	} {
		var got Route
		for _, route := range CurrentRoutes() {
			if route.ID == test.pattern {
				got = route
				break
			}
		}
		if got.ID == "" {
			t.Fatalf("route absent: %s", test.pattern)
		}
		if got.Class != test.class || got.OperatorOnly != test.operatorOnly {
			t.Errorf("%s: got %s, operator=%t; want %s, operator=%t", test.pattern,
				got.Class, got.OperatorOnly, test.class, test.operatorOnly)
		}
	}
}

func TestAgentExportDownloadTicketDoesNotQualifyArchiveBytes(t *testing.T) {
	const routeID = "POST /api/v1/exports/jobs/{id}/download"
	for _, operation := range CurrentOperations() {
		if !slices.Contains(operation.RouteIDs, routeID) {
			continue
		}
		if operation.ID != "browser_export_ticket" || !operation.OperatorOnly ||
			len(operation.CLIPaths) != 0 || len(operation.MCPTools) != 0 {
			t.Fatalf("one-use browser ticket cannot qualify as scoped archive-byte access: %+v", operation)
		}
		return
	}
	t.Fatalf("browser export ticket %s has no reviewed operator-only operation", routeID)
}
