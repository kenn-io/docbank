package agentops

import (
	"slices"
	"strings"
	"testing"
)

func TestAgentCurrentOperationsExposeRealBindingsAndGaps(t *testing.T) {
	routes := CurrentRoutes()
	operations := CurrentOperations()
	known := make(map[string]bool, len(routes))
	for _, route := range routes {
		known[route.ID] = true
	}
	for _, operation := range operations {
		if len(operation.RouteIDs) == 0 {
			t.Fatalf("operation %q has no route", operation.ID)
		}
		for _, id := range operation.RouteIDs {
			if !known[id] {
				t.Fatalf("operation %q references unknown route %s", operation.ID, id)
			}
		}
		if !operation.OperatorOnly && len(operation.CLIPaths) == 0 && !hasSurfaceGap(operation, "cli") {
			t.Fatalf("operation %q has no CLI binding or gap", operation.ID)
		}
		if !operation.OperatorOnly && len(operation.MCPTools) == 0 && !hasSurfaceGap(operation, "mcp") {
			t.Fatalf("operation %q has no MCP binding or gap", operation.ID)
		}
	}
	search := operationByID(t, operations, "search_documents")
	if !slices.Contains(search.CLIPaths, "search") || !slices.Contains(search.MCPTools, "search_documents") {
		t.Fatal("current search bindings missing")
	}
	formats := operationByID(t, operations, "format_coverage")
	if !slices.Contains(formats.CLIPaths, "formats") || !hasSurfaceGap(formats, "mcp") {
		t.Fatal("format coverage MCP gap hidden")
	}
	preflight := operationByID(t, operations, "package_preflight")
	if !preflight.OperatorOnly {
		t.Fatal("daemon-host path preflight is not operator-only")
	}
	prune := operationByID(t, operations, "prune_content_versions")
	if !prune.OperatorOnly || !slices.Equal(prune.RouteIDs, []string{"POST /api/v1/nodes/{id}/versions/prune"}) {
		t.Fatalf("content-version history prune must be an explicit operator-only operation: %+v", prune)
	}
	processing := operationByID(t, operations, "start_processing")
	if processing.Job.StatusRouteID != "GET /api/v1/processing/jobs/{id}" {
		t.Fatal("processing job status route is unspecified")
	}
	report := operationByID(t, operations, "report_create")
	if report.Receipt.Replay != "new-observation" {
		t.Fatal("frozen report rerun semantics omitted")
	}
	artifact := operationByID(t, operations, "report_artifact_download")
	if !strings.Contains(surfaceGapReason(artifact, "cli"), "standalone") ||
		!strings.Contains(surfaceGapReason(artifact, "mcp"), "verified artifact") {
		t.Fatal("report artifact parity gap is not explicit")
	}
}

func operationByID(t *testing.T, operations []Operation, id string) Operation {
	t.Helper()
	for _, operation := range operations {
		if operation.ID == id {
			return operation
		}
	}
	t.Fatalf("missing operation %s", id)
	return Operation{}
}

func hasSurfaceGap(operation Operation, surface string) bool {
	for _, gap := range operation.SurfaceGaps {
		if gap.Surface == surface && gap.Reason != "" {
			return true
		}
	}
	return false
}

func surfaceGapReason(operation Operation, surface string) string {
	for _, gap := range operation.SurfaceGaps {
		if gap.Surface == surface {
			return gap.Reason
		}
	}
	return ""
}

func TestAgentMultiRouteArtifactIsOneDomainOperation(t *testing.T) {
	operations := CurrentOperations()
	artifact := operationByID(t, operations, "report_artifact_download")
	for _, routeID := range []string{
		"GET /api/v1/search-exports/{id}/csv",
		"GET /api/v1/search-exports/{id}/bundle",
	} {
		if !slices.Contains(artifact.RouteIDs, routeID) {
			t.Fatalf("artifact operation omits %s", routeID)
		}
		for _, operation := range operations {
			if operation.ID == routeID {
				t.Fatalf("raw route %s falsely counted as a domain operation", routeID)
			}
		}
	}
	if !strings.Contains(surfaceGapReason(artifact, "cli"), "standalone") ||
		!strings.Contains(surfaceGapReason(artifact, "mcp"), "verified artifact") {
		t.Fatal("one domain operation does not own its current adapter gaps")
	}
}

func TestAgentUnreviewedDomainActionKeepsExplicitContractGap(t *testing.T) {
	for _, operation := range CurrentOperations() {
		if len(operation.RouteIDs) != 1 || operation.RouteIDs[0] != "GET /api/v1/collections" {
			continue
		}
		if operation.Feature != "unqualified" || !hasSurfaceGap(operation, "contract") ||
			!hasSurfaceGap(operation, "cli") || !hasSurfaceGap(operation, "mcp") {
			t.Fatalf("unreviewed collections operation silently qualified: %+v", operation)
		}
		return
	}
	t.Fatal("collections action missing from operation inventory")
}
