package agentapi

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"go.kenn.io/docbank/document/agentops"
)

func TestAgentCapabilityProjectionUsesRegistryAndRejectsMissingReadiness(t *testing.T) {
	routes := []agentops.Route{{ID: "GET /api/v1/formats/capabilities", Method: "GET",
		Pattern: "/api/v1/formats/capabilities", Class: agentops.Read}}
	operations := []agentops.Operation{{ID: "formats", RouteIDs: []string{routes[0].ID},
		CLIPaths: []string{"formats"}, SurfaceGaps: []agentops.SurfaceGap{{Surface: "mcp", Reason: "no tool"}}}}
	registry, err := New(routes, operations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildCapabilities(registry, "vault", "v1", []agentops.Feature{{Name: "formats", State: "unconfigured"}}); err == nil {
		t.Fatal("unexplained unavailable feature accepted")
	}
	capabilities, err := BuildCapabilities(registry, "vault", "v1", []agentops.Feature{{Name: "formats", State: "available"}})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := registry.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Schema != agentops.Schema || capabilities.Server.RegistryDigest != digest ||
		len(capabilities.Server.Routes) != 1 || len(capabilities.Server.Operations) != 0 {
		t.Fatalf("projection omitted authority: %+v", capabilities)
	}
	capabilities.Server.Routes[0].Pattern = "/forged"
	if registry.Routes()[0].Pattern != "/api/v1/formats/capabilities" {
		t.Fatal("projection mutated registry")
	}
}

func TestAgentCapabilityProjectionHidesUnqualifiedOperationLabels(t *testing.T) {
	routes := []agentops.Route{
		{ID: "formats", Method: "GET", Pattern: "/api/v1/formats/capabilities", Class: agentops.Read},
		{ID: "collections", Method: "GET", Pattern: "/api/v1/collections", Class: agentops.Read},
	}
	operations := []agentops.Operation{
		{ID: "format_coverage", RouteIDs: []string{"formats"}},
		{ID: "unqualified_get_collections", RouteIDs: []string{"collections"}, Feature: "unqualified"},
	}
	registry, err := New(routes, operations)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := BuildCapabilities(registry, "vault", "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities.Server.Operations) != 0 {
		t.Fatalf("unreviewed or incomplete labels leaked into discovery: %+v", capabilities.Server.Operations)
	}
}

func TestAgentCapabilitiesPublishReviewedNativeOperationsAndRetainCensus(t *testing.T) {
	routes, operations := agentops.CurrentRoutes(), agentops.CurrentOperations()
	registry, err := New(routes, operations)
	if err != nil {
		t.Fatal(err)
	}
	var named int
	for _, operation := range operations {
		if operation.Feature != "unqualified" {
			named++
		}
	}
	if named != 33 {
		t.Fatalf("expected the current 33 named operations, got %d", named)
	}
	before := GapInventory(registry.Operations())
	capabilities, err := BuildCapabilities(registry, "synthetic-vault", "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities.Server.Operations) != 28 {
		t.Fatalf("expected 28 reviewed native document, report and export operations, got %d", len(capabilities.Server.Operations))
	}
	for _, operation := range capabilities.Server.Operations {
		if !operation.ReviewedBehavior() {
			t.Fatalf("incomplete operation was advertised: %+v", operation)
		}
	}
	if !reflect.DeepEqual(capabilities.Server.Routes, registry.Routes()) {
		t.Fatal("withholding operation labels dropped route census authority")
	}
	if !reflect.DeepEqual(GapInventory(registry.Operations()), before) {
		t.Fatal("withholding operation labels erased internal parity gaps")
	}
}

func TestAgentCapabilitiesDoNotInferFalseBehavioralFlagsFromZeroValues(t *testing.T) {
	route := agentops.Route{ID: "POST /api/v1/synthetic/archive", Method: "POST",
		Pattern: "/api/v1/synthetic/archive", Class: agentops.Write}
	operation := agentops.Operation{ID: "synthetic_archive", RouteIDs: []string{route.ID},
		CLIPaths: []string{"synthetic archive"}, MCPTools: []string{"synthetic_archive"},
		Bounds:   agentops.Bounds{Paging: "none", ResponseBytes: 2048},
		InputRef: "SyntheticArchiveRequest", OutputRef: "SyntheticArchiveReceipt"}
	registry, err := New([]agentops.Route{route}, []agentops.Operation{operation})
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := BuildCapabilities(registry, "synthetic-vault", "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities.Server.Operations) != 0 {
		t.Fatal("false defaults for idempotence, destructiveness and open-world status were advertised as reviewed")
	}
	if len(registry.Operations()) != 1 {
		t.Fatal("incomplete operation was lost from the private registry")
	}
}

const reviewedSyntheticOperation = `{
	"id":"synthetic_list",
	"route_ids":["GET /api/v1/synthetic/items"],
	"cli_paths":["synthetic list"],
	"mcp_tools":["synthetic_list"],
	"qualification":"reviewed",
	"bounds":{"paging":"offset","default":20,"maximum":100,"response_bytes":8192},
	"input_ref":"SyntheticListRequest",
	"output_ref":"SyntheticListPage",
	"idempotent":true,
	"destructive":false,
	"open_world":false
}`

func reviewedSyntheticRegistry(t *testing.T, operation agentops.Operation) *Registry {
	t.Helper()
	route := agentops.Route{ID: "GET /api/v1/synthetic/items", Method: "GET",
		Pattern: "/api/v1/synthetic/items", Class: agentops.Read}
	registry, err := New([]agentops.Route{route}, []agentops.Operation{operation})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestAgentCapabilitiesPublishExplicitlyReviewedCompleteOperation(t *testing.T) {
	var operation agentops.Operation
	if err := json.Unmarshal([]byte(reviewedSyntheticOperation), &operation); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"qualification":"reviewed"`) {
		t.Fatal("explicit qualification marker was lost; a zero-valued behavior cannot prove review")
	}
	registry := reviewedSyntheticRegistry(t, operation)
	if gaps := GapInventory(registry.Operations()); len(gaps) != 0 {
		t.Fatalf("complete reviewed operation retained an artificial gap: %+v", gaps)
	}
	capabilities, err := BuildCapabilities(registry, "synthetic-vault", "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities.Server.Operations) != 1 ||
		!reflect.DeepEqual(capabilities.Server.Operations[0], operation) {
		t.Fatalf("complete reviewed operation was withheld or changed: %+v", capabilities.Server.Operations)
	}
}

func TestAgentCapabilitiesFailClosedOnInvalidReviewedMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*agentops.Operation)
	}{
		{"missing input", func(operation *agentops.Operation) { operation.InputRef = "" }},
		{"missing output", func(operation *agentops.Operation) { operation.OutputRef = "" }},
		{"unbounded response", func(operation *agentops.Operation) { operation.Bounds.ResponseBytes = 0 }},
		{"unbounded page", func(operation *agentops.Operation) { operation.Bounds.Maximum = 0 }},
		{"default exceeds maximum", func(operation *agentops.Operation) { operation.Bounds.Default = 101 }},
		{"unknown paging mode", func(operation *agentops.Operation) { operation.Bounds.Paging = "unbounded" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var operation agentops.Operation
			if err := json.Unmarshal([]byte(reviewedSyntheticOperation), &operation); err != nil {
				t.Fatal(err)
			}
			test.change(&operation)
			gaps := GapInventory([]agentops.Operation{operation})
			if len(gaps) != 1 || gaps[0].Surface != "contract" {
				t.Fatalf("invalid reviewed metadata lost its contract gap: %+v", gaps)
			}
			registry, err := New([]agentops.Route{{ID: "GET /api/v1/synthetic/items", Method: "GET",
				Pattern: "/api/v1/synthetic/items", Class: agentops.Read}}, []agentops.Operation{operation})
			if err != nil {
				return // Rejecting an invalid reviewed descriptor at registration is fail closed.
			}
			capabilities, err := BuildCapabilities(registry, "synthetic-vault", "v1", nil)
			if err == nil && len(capabilities.Server.Operations) != 0 {
				t.Fatalf("invalid reviewed metadata was advertised: %+v", capabilities.Server.Operations)
			}
		})
	}
}

func TestAgentCapabilitiesRejectUnknownQualification(t *testing.T) {
	raw := strings.Replace(reviewedSyntheticOperation,
		`"qualification":"reviewed"`, `"qualification":"candidate"`, 1)
	var operation agentops.Operation
	if err := json.Unmarshal([]byte(raw), &operation); err != nil {
		t.Fatal(err)
	}
	if gaps := GapInventory([]agentops.Operation{operation}); len(gaps) != 1 || gaps[0].Surface != "contract" {
		t.Fatalf("unknown qualification lost its contract gap: %+v", gaps)
	}
	registry := reviewedSyntheticRegistry(t, operation)
	capabilities, err := BuildCapabilities(registry, "synthetic-vault", "v1", nil)
	if err == nil && len(capabilities.Server.Operations) != 0 {
		t.Fatal("unknown qualification marker was advertised")
	}
}
