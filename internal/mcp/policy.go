package mcp

import (
	"context"

	"go.kenn.io/docbank/internal/api"
)

type operationPolicy struct {
	policy    *api.OperationPolicy
	principal api.Principal
}

func newOperationPolicy(policy *api.OperationPolicy, principal api.Principal) operationPolicy {
	if policy == nil {
		policy = api.NewOperationPolicy(api.OperationPolicyOptions{})
	}
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
