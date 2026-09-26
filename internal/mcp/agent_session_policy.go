package mcp

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type agentSessionGrantAuthority struct{ connection *daemonconn.Connection }

func (a agentSessionGrantAuthority) CurrentGrant(ctx context.Context, subjectID string) (api.Principal, error) {
	if a.connection == nil {
		return api.Principal{}, api.ErrOperationGrantRevoked
	}
	principal, err := a.connection.AgentSessionPrincipal(ctx)
	if err != nil || principal.SubjectID != subjectID {
		return api.Principal{}, api.ErrOperationGrantRevoked
	}
	return principal, nil
}

// ScopedAgentSessionOptions binds MCP to the daemon's live, read-only grant.
// The private session file identifies the credential; the daemon supplies its
// source fence and rechecks it on every authorization.
func ScopedAgentSessionOptions(ctx context.Context, connection *daemonconn.Connection, options ServerOptions) (ServerOptions, error) {
	if connection == nil {
		return ServerOptions{}, errors.New("agent session connection is unavailable")
	}
	principal, err := connection.AgentSessionPrincipal(ctx)
	if err != nil {
		return ServerOptions{}, err
	}
	options.Principal = principal
	options.OperationPolicy = api.NewOperationPolicy(api.OperationPolicyOptions{
		Authority: agentSessionGrantAuthority{connection: connection},
	})
	options.ScopedAgentSession = true
	options.AllowProcessing, options.AllowPackageWrites = false, false
	options.AllowExportWrites, options.AllowReportWrites = false, false
	return options, nil
}
