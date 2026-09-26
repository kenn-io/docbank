package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/document/agentops"
	"go.kenn.io/docbank/internal/agentapi"
	"go.kenn.io/docbank/internal/version"
)

const maxAgentCapabilitiesBytes = 1 << 20

// AgentCapabilities gives the agent contract a distinct OpenAPI schema name
// from the remote-target capability negotiation response.
type AgentCapabilities struct {
	Contract string                      `json:"schema"`
	Server   agentops.ServerCapabilities `json:"server"`
}

func registerAgentCapabilitiesRoute(api huma.API, d Deps, registry *agentapi.Registry) {
	type output struct {
		CacheControl string `header:"Cache-Control"`
		Body         AgentCapabilities
	}
	huma.Register(api, huma.Operation{
		OperationID: "readAgentCapabilities", Method: http.MethodGet,
		Path: "/api/v1/agent/capabilities", Summary: "Read currently available agent operations",
	}, func(ctx context.Context, _ *struct{}) (*output, error) {
		if d.Store == nil || registry == nil {
			return nil, NewError(http.StatusServiceUnavailable, "agent_capabilities_unavailable", "agent capabilities are unavailable")
		}
		if _, err := authorizeRequest(ctx, d, OperationRead, nil, false, false); err != nil {
			return nil, err
		}
		features := []agentops.Feature{
			capabilityFeature("native_documents", true, ""),
			capabilityFeature("native_formats", true, ""),
			capabilityFeature("native_processing", d.Processing != nil, "document processing is not configured"),
			capabilityFeature("native_reports", d.Blobs != nil, "report blob authority is not configured"),
			capabilityFeature("native_exports", d.Exports != nil, "export worker is not configured"),
		}
		capabilities, err := agentapi.BuildCapabilities(registry, d.Store.VaultID(), version.Version, features)
		if err != nil {
			return nil, NewError(http.StatusServiceUnavailable, "agent_capabilities_unavailable", "agent capabilities are unavailable")
		}
		principal, _ := PrincipalFromContext(ctx)
		capabilities = availableAgentCapabilities(capabilities, principal)
		raw, err := json.Marshal(capabilities)
		if err != nil || len(raw) > maxAgentCapabilitiesBytes {
			return nil, NewError(http.StatusServiceUnavailable, "agent_capabilities_unavailable", "agent capabilities exceed the response limit")
		}
		return &output{CacheControl: "no-store", Body: AgentCapabilities{
			Contract: capabilities.Schema, Server: capabilities.Server,
		}}, nil
	})
}

func capabilityFeature(name string, available bool, reason string) agentops.Feature {
	if available {
		return agentops.Feature{Name: name, State: "available"}
	}
	return agentops.Feature{Name: name, State: "unavailable", Reason: reason}
}

// The digest identifies the full reviewed registry; a scoped response exposes
// only routes the caller can enter and operations whose every route is usable.
func availableAgentCapabilities(capabilities agentops.Capabilities, principal Principal) agentops.Capabilities {
	available := make(map[string]bool, len(capabilities.Server.Features))
	for _, feature := range capabilities.Server.Features {
		available[feature.Name] = feature.State == "available"
	}
	allowedRoutes := make(map[string]bool, len(capabilities.Server.Routes))
	for _, route := range capabilities.Server.Routes {
		if !principal.Local && (route.OperatorOnly || route.Class != agentops.Read ||
			!scopedRequestAllowed(route.Method, route.Pattern)) {
			continue
		}
		allowedRoutes[route.ID] = true
	}
	operations := make([]agentops.Operation, 0, len(capabilities.Server.Operations))
	visibleRoutes := make(map[string]bool)
	for _, operation := range capabilities.Server.Operations {
		if !available[operation.Feature] || (!principal.Local &&
			!slices.Contains(principal.Operations, agentCapabilityGrant(operation.ID))) {
			continue
		}
		if !slices.ContainsFunc(operation.RouteIDs, func(id string) bool { return !allowedRoutes[id] }) {
			operations = append(operations, operation)
			for _, id := range operation.RouteIDs {
				visibleRoutes[id] = true
			}
		}
	}
	routes := capabilities.Server.Routes
	if !principal.Local {
		routes = make([]agentops.Route, 0, len(visibleRoutes))
		for _, route := range capabilities.Server.Routes {
			if visibleRoutes[route.ID] {
				routes = append(routes, route)
			}
		}
	}
	capabilities.Server.Routes, capabilities.Server.Operations = routes, operations
	return capabilities
}

func agentCapabilityGrant(id string) Operation {
	switch id {
	case "get_processing_plan", "get_processing_status":
		return OperationProcessing
	case "search_documents", "get_processing_coverage":
		return OperationAnalyze
	default:
		return OperationRead
	}
}
