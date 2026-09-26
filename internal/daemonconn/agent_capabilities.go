package daemonconn

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"go.kenn.io/docbank/document/agentops"
	"go.kenn.io/docbank/internal/agentapi"
)

// AgentCapabilities reads the caller's live capability projection. The daemon
// enforces its current credential and source grant before producing it.
func (c *Connection) AgentCapabilities(ctx context.Context) (agentops.Capabilities, error) {
	response, err := c.API().ReadAgentCapabilities(ctx)
	if err != nil {
		return agentops.Capabilities{}, err
	}
	capabilities := agentops.Capabilities{Schema: response.Contract, Server: response.Server}
	if err := capabilities.Validate(); err != nil {
		return agentops.Capabilities{}, err
	}
	if capabilities.Server.VaultID == "" || capabilities.Server.Version == "" ||
		len(capabilities.Server.Routes) > 512 || len(capabilities.Server.Operations) > 256 ||
		len(capabilities.Server.Features) > 32 {
		return agentops.Capabilities{}, errors.New("agent capability response exceeds its authority bounds")
	}
	digest, ok := strings.CutPrefix(capabilities.Server.RegistryDigest, "sha256:")
	if !ok || len(digest) != 64 {
		return agentops.Capabilities{}, errors.New("agent capability response has an invalid registry digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return agentops.Capabilities{}, errors.New("agent capability response has an invalid registry digest")
	}
	if _, err := agentapi.New(capabilities.Server.Routes, capabilities.Server.Operations); err != nil {
		return agentops.Capabilities{}, errors.New("agent capability response has inconsistent routes or operations")
	}
	return capabilities, nil
}
