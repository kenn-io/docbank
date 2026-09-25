package agentapi

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/document/agentops"
)

// Gap is an unbound or unreviewed agent surface for one domain operation.
// RouteIDs are evidence for that operation, not separate parity obligations.
type Gap struct {
	OperationID string   `json:"operation_id"`
	Surface     string   `json:"surface"`
	Reason      string   `json:"reason"`
	RouteIDs    []string `json:"route_ids"`
}

// GapInventory returns a stable, operation-scoped list for integration gates.
// An unqualified route remains a contract gap even if adapters are later bound.
func GapInventory(operations []agentops.Operation) []Gap {
	var gaps []Gap
	for _, operation := range operations {
		if operation.OperatorOnly {
			continue
		}
		bySurface := make(map[string]string)
		for _, gap := range operation.SurfaceGaps {
			bySurface[gap.Surface] = gap.Reason
		}
		if operation.Feature == "unqualified" {
			if _, ok := bySurface["contract"]; !ok {
				bySurface["contract"] = "Domain operation contract requires review"
			}
		} else if !operation.ReviewedBehavior() {
			if _, ok := bySurface["contract"]; !ok {
				bySurface["contract"] = "Behavioral metadata requires review"
			}
		}
		if len(operation.CLIPaths) == 0 {
			if _, ok := bySurface["cli"]; !ok {
				bySurface["cli"] = "No equivalent CLI operation"
			}
		}
		if len(operation.MCPTools) == 0 {
			if _, ok := bySurface["mcp"]; !ok {
				bySurface["mcp"] = "No typed MCP operation"
			}
		}
		for surface, reason := range bySurface {
			routeIDs := slices.Clone(operation.RouteIDs)
			slices.Sort(routeIDs)
			gaps = append(gaps, Gap{
				OperationID: operation.ID, Surface: surface,
				Reason: reason, RouteIDs: routeIDs,
			})
		}
	}
	slices.SortFunc(gaps, func(a, b Gap) int {
		if comparison := strings.Compare(a.OperationID, b.OperationID); comparison != 0 {
			return comparison
		}
		return strings.Compare(a.Surface, b.Surface)
	})
	return gaps
}

// ValidateCoverage requires every eligible daemon route to belong to exactly
// one domain operation. Operator-only routes are explicit reviewed exclusions
// in the route census; if modeled, they must remain operator-only there too.
func ValidateCoverage(routes []agentops.Route, operations []agentops.Operation) error {
	byID := make(map[string]agentops.Route, len(routes))
	for _, route := range routes {
		if route.ID == "" || byID[route.ID].ID != "" {
			return fmt.Errorf("duplicate or empty route %q", route.ID)
		}
		byID[route.ID] = route
	}
	count := make(map[string]int, len(routes))
	for _, operation := range operations {
		for _, id := range operation.RouteIDs {
			route, ok := byID[id]
			if !ok {
				return fmt.Errorf("operation %q references missing route %q", operation.ID, id)
			}
			if route.OperatorOnly != operation.OperatorOnly {
				return fmt.Errorf("operation %q disagrees with operator-only route %q", operation.ID, id)
			}
			count[id]++
			if count[id] > 1 {
				return fmt.Errorf("route %q belongs to more than one operation", id)
			}
		}
	}
	for _, route := range routes {
		if !route.OperatorOnly && count[route.ID] != 1 {
			return fmt.Errorf("eligible route %q lacks a domain operation", route.ID)
		}
	}
	return nil
}

// ValidateParity checks domain operations, not raw HTTP routes. A caller may
// use it as the final qualification gate after adapter work is integrated.
func ValidateParity(operations []agentops.Operation) error {
	seen := make(map[string]bool, len(operations))
	for _, operation := range operations {
		if operation.ID == "" || seen[operation.ID] {
			return errors.New("duplicate or empty domain operation")
		}
		seen[operation.ID] = true
	}
	var gaps []string
	for _, gap := range GapInventory(operations) {
		gaps = append(gaps, gap.OperationID+":"+gap.Surface)
	}
	if len(gaps) == 0 {
		return nil
	}
	return errors.New("agent operation parity gaps: " + strings.Join(gaps, ", "))
}
