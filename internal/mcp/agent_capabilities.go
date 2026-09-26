package mcp

import (
	"context"

	"go.kenn.io/docbank/document/agentops"
	"go.kenn.io/docbank/internal/daemonconn"
)

type agentCapabilityOperation struct {
	ID            string   `json:"id"`
	Feature       string   `json:"feature"`
	CLIPaths      []string `json:"cli_paths"`
	MCPTools      []string `json:"mcp_tools"`
	Paging        string   `json:"paging"`
	MaximumPage   int      `json:"maximum_page"`
	ResponseBytes int64    `json:"response_bytes"`
}

type agentCapabilitiesOutput struct {
	privateCache

	Schema         string                     `json:"schema"`
	VaultID        string                     `json:"vault_id"`
	Version        string                     `json:"version"`
	RegistryDigest string                     `json:"registry_digest"`
	Features       []agentops.Feature         `json:"features"`
	Operations     []agentCapabilityOperation `json:"operations"`
}

func getAgentCapabilities(ctx context.Context, lease *daemonLease, raw []byte) (agentCapabilitiesOutput, error) {
	var input struct{}
	if err := decodeReadArguments(raw, &input); err != nil {
		return agentCapabilitiesOutput{}, err
	}
	capabilities, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (agentops.Capabilities, error) {
		return c.AgentCapabilities(ctx)
	})
	if err != nil {
		return agentCapabilitiesOutput{}, err
	}
	result := agentCapabilitiesOutput{
		privateCache: newPrivateCache(), Schema: capabilities.Schema,
		VaultID: capabilities.Server.VaultID, Version: capabilities.Server.Version,
		RegistryDigest: capabilities.Server.RegistryDigest,
		Features:       append([]agentops.Feature{}, capabilities.Server.Features...),
		Operations:     make([]agentCapabilityOperation, 0, len(capabilities.Server.Operations)),
	}
	for _, operation := range capabilities.Server.Operations {
		result.Operations = append(result.Operations, agentCapabilityOperation{
			ID: operation.ID, Feature: operation.Feature,
			CLIPaths: append([]string{}, operation.CLIPaths...),
			MCPTools: append([]string{}, operation.MCPTools...),
			Paging:   operation.Bounds.Paging, MaximumPage: operation.Bounds.Maximum,
			ResponseBytes: operation.Bounds.ResponseBytes,
		})
	}
	return result, nil
}

func getAgentCapabilitiesSchemas() (schema, schema) {
	input := rootObjectSchema(schema{})
	digest := stringSchema(71)
	digest["pattern"] = "^sha256:[0-9a-f]{64}$"
	feature := objectSchema(schema{
		"name": stringSchema(128), "state": enumSchema("available", "unavailable", "unconfigured"),
		"reason": stringSchema(256),
	}, "name", "state")
	operation := objectSchema(schema{
		"id": stringSchema(128), "feature": stringSchema(128),
		"cli_paths":      arraySchema(stringSchema(256), 16),
		"mcp_tools":      arraySchema(stringSchema(128), 16),
		"paging":         enumSchema("none", "offset", "cursor"),
		"maximum_page":   integerSchema(0, 4096),
		"response_bytes": integerSchema(1, 1<<30),
	}, "id", "feature", "cli_paths", "mcp_tools", "paging", "maximum_page", "response_bytes")
	output := rootObjectSchema(withPrivateCache(schema{
		"schema": enumSchema(agentops.Schema), "vault_id": uuidSchema(),
		"version":         stringSchema(128),
		"registry_digest": digest,
		"features":        arraySchema(feature, 32), "operations": arraySchema(operation, 256),
	}), cacheRequired("schema", "vault_id", "version", "registry_digest", "features", "operations")...)
	return input, output
}
