package agentapi

import (
	"strings"
	"testing"

	"go.kenn.io/docbank/document/agentops"
)

func TestOperationRegistryCensusRejectsMissingRawWrite(t *testing.T) {
	routes := []agentops.Route{{ID: "info", Method: "GET", Pattern: "/api/v1/info", Class: agentops.Read}}
	actual := []string{"GET /api/v1/info", "PUT /api/v1/nodes/{id}/content"}
	err := ValidateCensus(actual, routes)
	if err == nil || !strings.Contains(err.Error(), "PUT /api/v1/nodes/{id}/content") {
		t.Fatalf("missing raw write not reported: %v", err)
	}
	routes = append(routes, agentops.Route{ID: "content", Method: "PUT", Pattern: "/api/v1/nodes/{id}/content", Class: agentops.Write})
	if err := ValidateCensus(actual, routes); err != nil {
		t.Fatal(err)
	}
	routes[1].Class = agentops.Read
	if err := ValidateCensus(actual, routes); err == nil {
		t.Fatal("write classified read")
	}
}
