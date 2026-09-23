package mcp

import (
	"context"
	"log/slog"

	"go.kenn.io/docbank/internal/api"
)

type operationPolicy struct {
	policy    *api.OperationPolicy
	principal api.Principal
}

func newOperationPolicy(policy *api.OperationPolicy, principal api.Principal, loggers ...*slog.Logger) operationPolicy {
	var logger *slog.Logger
	if len(loggers) != 0 {
		logger = loggers[0]
	}
	policy = policy.WithAuditIfAbsent(api.NewLogOperationAudit(logger))
	if principal.SubjectID == "" {
		principal = api.LocalAdminPrincipal()
	}
	return operationPolicy{policy: policy, principal: principal}
}

func (p operationPolicy) authorize(ctx context.Context, operation api.Operation, sourceIDs []string, requireAll, protected bool) (api.OperationAuthorization, error) {
	return p.policy.Authorize(ctx, api.OperationAuthorizationRequest{
		Principal: p.principal, Operation: operation, SourceIDs: sourceIDs,
		RequireAll: requireAll, Protected: protected,
	})
}

func (p operationPolicy) local() bool { return p.principal.Local }
