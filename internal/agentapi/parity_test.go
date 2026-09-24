package agentapi

import (
	"strings"
	"testing"

	"go.kenn.io/docbank/document/agentops"
)

func TestAgentOperationParityReportsOneGapPerDomainSurface(t *testing.T) {
	err := ValidateParity(agentops.CurrentOperations())
	if err == nil {
		t.Fatal("unbound domain operations passed parity")
	}
	message := err.Error()
	if strings.Count(message, "report_artifact_download:cli") != 1 ||
		strings.Count(message, "report_artifact_download:mcp") != 1 {
		t.Fatalf("multi-route artifact counted by raw route instead of operation: %s", message)
	}
	if strings.Contains(message, "GET /api/v1/search-exports/{id}/csv:") ||
		strings.Contains(message, "GET /api/v1/search-exports/{id}/bundle:") {
		t.Fatalf("raw route leaked into parity key: %s", message)
	}
	if !strings.Contains(message, "format_coverage:mcp") {
		t.Fatal("format MCP gap hidden")
	}
	if !strings.Contains(message, "unqualified_get_collections:contract") {
		t.Fatal("unreviewed domain contract silently passed parity")
	}
}

func TestAgentEveryEligibleRouteHasOneDomainOperation(t *testing.T) {
	if err := ValidateCoverage(agentops.CurrentRoutes(), agentops.CurrentOperations()); err != nil {
		t.Fatal(err)
	}
}

func TestAgentGapInventoryIsStableAndOperationScoped(t *testing.T) {
	gaps := GapInventory(agentops.CurrentOperations())
	var reportCLI, reportMCP, collectionContract int
	for _, gap := range gaps {
		switch gap.OperationID + ":" + gap.Surface {
		case "report_artifact_download:cli":
			reportCLI++
		case "report_artifact_download:mcp":
			reportMCP++
		case "unqualified_get_collections:contract":
			collectionContract++
		}
	}
	if reportCLI != 1 || reportMCP != 1 || collectionContract != 1 {
		t.Fatalf("missing or duplicate operation gaps: report CLI=%d MCP=%d collection=%d", reportCLI, reportMCP, collectionContract)
	}
	for i := 1; i < len(gaps); i++ {
		previous, current := gaps[i-1], gaps[i]
		if previous.OperationID > current.OperationID || previous.OperationID == current.OperationID && previous.Surface > current.Surface {
			t.Fatal("gap inventory is not stable")
		}
	}
	for _, gap := range gaps {
		t.Logf("%s:%s routes=%s reason=%s", gap.OperationID, gap.Surface, strings.Join(gap.RouteIDs, ","), gap.Reason)
	}
}

func TestAgentIncompleteNamedOperationsRemainContractGaps(t *testing.T) {
	operations := agentops.CurrentOperations()
	gaps := GapInventory(operations)
	contracts := make(map[string]Gap)
	for _, gap := range gaps {
		if gap.Surface == "contract" {
			contracts[gap.OperationID] = gap
		}
	}
	var provisional, namedEligible int
	for _, operation := range operations {
		if operation.Feature == "unqualified" {
			provisional++
			continue
		}
		if operation.OperatorOnly {
			continue // Reviewed operator-only exclusions do not enter parity.
		}
		namedEligible++
		gap, ok := contracts[operation.ID]
		if !ok || !strings.Contains(gap.Reason, "metadata") ||
			len(gap.RouteIDs) != len(operation.RouteIDs) {
			t.Errorf("incomplete named operation %s lacks its metadata contract gap: %+v",
				operation.ID, gap)
		}
	}
	if provisional != 151 || namedEligible == 0 {
		t.Fatalf("provisional identity or named eligible census drifted: provisional=%d named=%d",
			provisional, namedEligible)
	}
}

func TestAgentGapInventoryUsesQualificationForSyntheticNamedOperation(t *testing.T) {
	operation := agentops.Operation{
		ID: "synthetic_archive", RouteIDs: []string{"POST /api/v1/synthetic/archive"},
		CLIPaths: []string{"synthetic archive"}, MCPTools: []string{"synthetic_archive"},
		Bounds:   agentops.Bounds{Paging: "offset", Default: 20, Maximum: 100, ResponseBytes: 8192},
		InputRef: "SyntheticArchiveRequest", OutputRef: "SyntheticArchiveReceipt",
	}
	gaps := GapInventory([]agentops.Operation{operation})
	if len(gaps) != 1 || gaps[0].Surface != "contract" ||
		!strings.Contains(gaps[0].Reason, "metadata") {
		t.Fatalf("named operation without explicit behavior qualification lost its contract gap: %+v", gaps)
	}
}
