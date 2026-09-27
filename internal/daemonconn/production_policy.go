package daemonconn

import (
	"context"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	productionservice "go.kenn.io/docbank/internal/production"
)

func (c *Connection) CreateProductionPolicyVersion(ctx context.Context,
	request api.ProductionPolicyCreateRequest) (documentproduction.PolicyVersion, error) {
	prepared, err := productionservice.PreparePolicyVersion(request.OperationID, request.Policy)
	if err != nil {
		return documentproduction.PolicyVersion{}, err
	}
	policy, err := c.API().CreateProductionPolicyVersion(ctx,
		&apiclient.CreateProductionPolicyVersionRequestOptions{Body: &request})
	if err != nil {
		return documentproduction.PolicyVersion{}, err
	}
	if policy == nil || policy.ID != request.Policy.ID || policy.Version != request.Policy.Version ||
		policy.SHA256 != prepared.PolicySHA256 ||
		documentproduction.ValidatePolicyVersion(*policy) != nil {
		return documentproduction.PolicyVersion{}, integrityErrorf("production policy receipt is inconsistent")
	}
	return *policy, nil
}

func (c *Connection) ProductionPolicyVersion(ctx context.Context,
	policyID string, version int64) (documentproduction.PolicyVersion, error) {
	if policyID == "" || version < 1 {
		return documentproduction.PolicyVersion{}, errors.New("invalid production policy reference")
	}
	policy, err := c.API().ReadProductionPolicyVersion(ctx,
		&apiclient.ReadProductionPolicyVersionRequestOptions{PathParams: &apiclient.ReadProductionPolicyVersionPath{
			PolicyID: policyID, Version: version}})
	if err != nil {
		return documentproduction.PolicyVersion{}, err
	}
	if policy == nil || policy.ID != policyID || policy.Version != version ||
		documentproduction.ValidatePolicyVersion(*policy) != nil {
		return documentproduction.PolicyVersion{}, integrityErrorf("production policy read is inconsistent")
	}
	return *policy, nil
}
