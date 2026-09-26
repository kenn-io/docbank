package agentapi

import (
	"errors"

	"go.kenn.io/docbank/document/agentops"
)

// BuildCapabilities projects a validated registry and caller-supplied runtime
// readiness. A daemon route can call this only after checking actual services.
func BuildCapabilities(registry *Registry, vaultID, version string, features []agentops.Feature) (agentops.Capabilities, error) {
	if registry == nil || vaultID == "" || version == "" {
		return agentops.Capabilities{}, errors.New("agent capability authority is unavailable")
	}
	digest, err := registry.Digest()
	if err != nil {
		return agentops.Capabilities{}, err
	}
	visibleOperations := make([]agentops.Operation, 0, len(registry.operations))
	for _, operation := range registry.Operations() {
		if operation.ReviewedBehavior() {
			visibleOperations = append(visibleOperations, operation)
		}
	}
	capabilities := agentops.Capabilities{Schema: agentops.Schema, Server: agentops.ServerCapabilities{
		VaultID: vaultID, Version: version, RegistryDigest: digest,
		Routes: registry.Routes(), Operations: visibleOperations,
		Features: append([]agentops.Feature(nil), features...),
	}}
	if err := capabilities.Validate(); err != nil {
		return agentops.Capabilities{}, err
	}
	return capabilities, nil
}
