// Package agentapi validates the operation graph used by daemon, CLI and MCP
// adapters. Registration does not itself grant access; the daemon enforces
// the matched route's class when a scoped credential is presented.
package agentapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/docbank/document/agentops"
	"go.kenn.io/docbank/internal/canonical"
)

type Registry struct {
	routes     []agentops.Route
	operations []agentops.Operation
	mux        *http.ServeMux
	patterns   map[string]agentops.Route
}

func New(routes []agentops.Route, operations []agentops.Operation) (registry *Registry, err error) {
	defer func() {
		if problem := recover(); problem != nil {
			registry = nil
			err = fmt.Errorf("invalid route registry: %v", problem)
		}
	}()
	registry = &Registry{mux: http.NewServeMux(), patterns: make(map[string]agentops.Route)}
	rawRoutes, err := canonical.Marshal(routes)
	if err != nil {
		return nil, err
	}
	registry.routes, err = canonical.Decode[[]agentops.Route](rawRoutes)
	if err != nil {
		return nil, err
	}
	rawOperations, err := canonical.Marshal(operations)
	if err != nil {
		return nil, err
	}
	registry.operations, err = canonical.Decode[[]agentops.Operation](rawOperations)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(routes))
	for _, route := range registry.routes {
		if route.ID == "" || ids[route.ID] || !validMethod(route.Method) ||
			!strings.HasPrefix(route.Pattern, "/") || !agentops.PolicyAdmin.Allows(route.Class) {
			return nil, fmt.Errorf("invalid route %q", route.ID)
		}
		ids[route.ID] = true
		pattern := route.Method + " " + route.Pattern
		if _, exists := registry.patterns[pattern]; exists {
			return nil, fmt.Errorf("duplicate route %q", pattern)
		}
		registry.patterns[pattern] = route
		registry.mux.Handle(pattern, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	}
	operationIDs := make(map[string]bool, len(operations))
	tools := make(map[string]bool)
	for _, operation := range registry.operations {
		if operation.ID == "" || operationIDs[operation.ID] || len(operation.RouteIDs) == 0 {
			return nil, fmt.Errorf("invalid operation %q", operation.ID)
		}
		operationIDs[operation.ID] = true
		for _, id := range operation.RouteIDs {
			if !ids[id] {
				return nil, fmt.Errorf("operation %q references missing route %q", operation.ID, id)
			}
		}
		for _, id := range []string{operation.Job.StatusRouteID, operation.Job.CancelRouteID} {
			if id != "" && !ids[id] {
				return nil, fmt.Errorf("operation %q references missing job route %q", operation.ID, id)
			}
		}
		for _, tool := range operation.MCPTools {
			if tool == "" || tools[tool] {
				return nil, fmt.Errorf("duplicate or empty MCP tool %q", tool)
			}
			tools[tool] = true
		}
	}
	slices.SortFunc(registry.routes, func(a, b agentops.Route) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(registry.operations, func(a, b agentops.Operation) int { return strings.Compare(a.ID, b.ID) })
	return registry, nil
}

func validMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func (registry *Registry) Match(request *http.Request) (agentops.Route, bool) {
	if registry == nil || request == nil {
		return agentops.Route{}, false
	}
	_, pattern := registry.mux.Handler(request)
	route, ok := registry.patterns[pattern]
	return route, ok
}

func (registry *Registry) Routes() []agentops.Route { return slices.Clone(registry.routes) }

func (registry *Registry) Operations() []agentops.Operation {
	raw, err := canonical.Marshal(registry.operations)
	if err != nil {
		panic(err)
	}
	out, err := canonical.Decode[[]agentops.Operation](raw)
	if err != nil {
		panic(err)
	}
	return out
}

func (registry *Registry) Digest() (string, error) {
	if registry == nil {
		return "", errors.New("nil registry")
	}
	raw, err := canonical.Marshal(struct {
		Routes     []agentops.Route     `json:"routes"`
		Operations []agentops.Operation `json:"operations"`
	}{registry.routes, registry.operations})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
