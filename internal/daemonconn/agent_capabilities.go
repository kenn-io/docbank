package daemonconn

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"go.kenn.io/docbank/document/agentops"
	"go.kenn.io/docbank/internal/agentapi"
	"go.kenn.io/docbank/internal/api"
)

var ErrAgentSessionGrant = errors.New("agent session grant is unavailable or invalid")

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

// AgentSessionPrincipal reads the daemon's live grant for this credential.
// A session file never supplies source IDs or authorization to the MCP policy.
func (c *Connection) AgentSessionPrincipal(ctx context.Context) (api.Principal, error) {
	if c == nil {
		return api.Principal{}, ErrAgentSessionGrant
	}
	response, err := c.API().ReadAgentCapabilities(ctx)
	if err != nil {
		return api.Principal{}, err
	}
	if response == nil || response.Contract != agentops.Schema || response.Session == nil ||
		!validUUIDv4(response.Server.VaultID) {
		return api.Principal{}, ErrAgentSessionGrant
	}
	session := response.Session
	id, ok := strings.CutPrefix(session.SubjectID, "agent:")
	if !ok || len(id) != 32 || strings.ToLower(id) != id ||
		session.CredentialKind != "agent_session" ||
		session.Audience != "docbank:"+response.Server.VaultID ||
		len(session.Operations) != 1 || session.Operations[0] != api.OperationRead ||
		session.GrantRevision == 0 || !session.ExpiresAt.After(time.Now()) ||
		len(session.SourceIDs) == 0 || len(session.SourceIDs) > api.MaxOperationSourceIDs {
		return api.Principal{}, ErrAgentSessionGrant
	}
	if _, err := hex.DecodeString(id); err != nil {
		return api.Principal{}, ErrAgentSessionGrant
	}
	seen := make(map[string]bool, len(session.SourceIDs))
	for _, sourceID := range session.SourceIDs {
		if !validUUIDv4(sourceID) || seen[sourceID] {
			return api.Principal{}, ErrAgentSessionGrant
		}
		seen[sourceID] = true
	}
	return api.Principal{
		SubjectID: session.SubjectID, CredentialKind: session.CredentialKind,
		Audience: session.Audience, Operations: append([]api.Operation(nil), session.Operations...),
		SourceIDs: append([]string(nil), session.SourceIDs...), GrantRevision: session.GrantRevision,
		ExpiresAt: session.ExpiresAt,
	}, nil
}
