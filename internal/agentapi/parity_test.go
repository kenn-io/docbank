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
	if strings.Contains(message, "report_artifact_download:") {
		t.Fatalf("reviewed report artifact still has a parity gap: %s", message)
	}
	if strings.Contains(message, "GET /api/v1/search-exports/{id}/csv:") ||
		strings.Contains(message, "GET /api/v1/search-exports/{id}/bundle:") {
		t.Fatalf("raw route leaked into parity key: %s", message)
	}
	if strings.Contains(message, "format_coverage:mcp") {
		t.Fatal("implemented format MCP tool still has a parity gap")
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
	var reportCLI, reportMCP, reportContract, collectionContract int
	for _, gap := range gaps {
		switch gap.OperationID + ":" + gap.Surface {
		case "report_artifact_download:cli":
			reportCLI++
		case "report_artifact_download:mcp":
			reportMCP++
		case "report_artifact_download:contract":
			reportContract++
		case "unqualified_get_collections:contract":
			collectionContract++
		}
	}
	if reportCLI != 0 || reportMCP != 0 || reportContract != 0 || collectionContract != 1 {
		t.Fatalf("incorrect operation gaps: report CLI=%d MCP=%d contract=%d collection=%d",
			reportCLI, reportMCP, reportContract, collectionContract)
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
	var provisional, namedEligible, reviewed int
	for _, operation := range operations {
		if operation.Feature == "unqualified" {
			provisional++
			continue
		}
		if operation.OperatorOnly {
			continue // Reviewed operator-only exclusions do not enter parity.
		}
		namedEligible++
		if operation.ReviewedBehavior() {
			reviewed++
			if _, exists := contracts[operation.ID]; exists {
				t.Errorf("reviewed operation %s still has a contract gap", operation.ID)
			}
			continue
		}
		gap, ok := contracts[operation.ID]
		if !ok || !strings.Contains(gap.Reason, "metadata") ||
			len(gap.RouteIDs) != len(operation.RouteIDs) {
			t.Errorf("incomplete named operation %s lacks its metadata contract gap: %+v",
				operation.ID, gap)
		}
	}
	if provisional != 141 || namedEligible != 26 || reviewed != 26 {
		t.Fatalf("provisional, named, or reviewed census drifted: provisional=%d named=%d reviewed=%d",
			provisional, namedEligible, reviewed)
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
