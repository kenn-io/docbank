package agentapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.kenn.io/docbank/document/agentops"
)

func TestOperationRegistryMatchesActualRouteWithoutMergingBindings(t *testing.T) {
	routes := []agentops.Route{{ID: "page", Method: http.MethodPost,
		Pattern: "/api/v1/workspace/queries/{id}/pages", Class: agentops.Read}}
	operations := []agentops.Operation{
		{ID: "snapshot-page", RouteIDs: []string{"page"}, MCPTools: []string{"list_query_snapshot_page"}},
		{ID: "timeline-page", RouteIDs: []string{"page"}, MCPTools: []string{"list_timeline_events"}, Feature: "timeline"},
	}
	registry, err := New(routes, operations)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/workspace/queries/example/pages", nil)
	got, ok := registry.Match(request)
	if !ok || got.ID != "page" {
		t.Fatalf("matched %+v, %v", got, ok)
	}
	if _, ok := registry.Match(httptest.NewRequest(http.MethodGet, request.URL.Path, nil)); ok {
		t.Fatal("wrong method matched")
	}
	if len(registry.Operations()) != 2 {
		t.Fatal("shared route discarded an operation")
	}

	digest, err := registry.Digest()
	if err != nil {
		t.Fatal(err)
	}
	registry.Operations()[0].RouteIDs[0] = "changed"
	operations[0].RouteIDs[0] = "changed"
	routes[0].Pattern = "/changed"
	after, err := registry.Digest()
	if err != nil || digest != after {
		t.Fatal("caller mutated registry")
	}
	if got, ok := registry.Match(request); !ok || got.ID != "page" {
		t.Fatal("caller changed matcher")
	}

	operations[0].RouteIDs[0] = "page"
	operations[1].RouteIDs = []string{"missing"}
	if _, err := New(routes, operations); err == nil {
		t.Fatal("dangling route accepted")
	}
}

func TestOperationRegistryRejectsUnclassifiedRoutes(t *testing.T) {
	for _, route := range []agentops.Route{
		{ID: "unknown", Method: "GET", Pattern: "/api/v1/unknown", Class: "unknown"},
		{ID: "missing", Method: "GET", Pattern: "/api/v1/missing", Class: agentops.Read},
	} {
		if route.ID == "missing" {
			route.ID = ""
		}
		if _, err := New([]agentops.Route{route}, nil); err == nil {
			t.Fatalf("accepted %+v", route)
		}
	}
}

func TestOperationRegistryRejectsDanglingJobStatusRoute(t *testing.T) {
	routes := []agentops.Route{{ID: "start", Method: "POST", Pattern: "/api/v1/jobs", Class: agentops.Write}}
	operations := []agentops.Operation{{ID: "start", RouteIDs: []string{"start"},
		Job: agentops.JobSemantics{StatusRouteID: "missing", Durable: true}}}
	if _, err := New(routes, operations); err == nil {
		t.Fatal("dangling status route accepted")
	}
}

func TestCurrentOperationRegistryBuildsFromExhaustiveCensus(t *testing.T) {
	registry, err := New(agentops.CurrentRoutes(), agentops.CurrentOperations())
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCoverage(registry.Routes(), registry.Operations()); err != nil {
		t.Fatal(err)
	}
}
