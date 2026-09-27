package daemonconn

import (
	"context"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

func (c *Connection) ProductionPolicyVersions(ctx context.Context,
	cursor string, limit int) (api.ProductionPolicyPage, error) {
	if len(cursor) > 60 || limit < 0 || limit > store.MaxProductionPolicyPage {
		return api.ProductionPolicyPage{}, errors.New("invalid production policy page")
	}
	requestedLimit := limit
	if requestedLimit == 0 {
		requestedLimit = 25
	}
	query := apiclient.ListProductionPolicyVersionsQuery{}
	if cursor != "" {
		query.Cursor = &cursor
	}
	if limit != 0 {
		value := int64(limit)
		query.Limit = &value
	}
	page, err := c.API().ListProductionPolicyVersions(ctx,
		&apiclient.ListProductionPolicyVersionsRequestOptions{Query: &query})
	if err != nil {
		return api.ProductionPolicyPage{}, err
	}
	if page == nil || len(page.Items) > requestedLimit ||
		page.NextCursor != "" && (len(page.NextCursor) > 60 || len(page.Items) != requestedLimit) {
		return api.ProductionPolicyPage{}, integrityErrorf("production policy page is inconsistent")
	}
	lastID := ""
	var lastVersion int64
	for _, policy := range page.Items {
		if documentproduction.ValidatePolicyVersion(policy) != nil || policy.ID < lastID ||
			policy.ID == lastID && policy.Version <= lastVersion {
			return api.ProductionPolicyPage{}, integrityErrorf("production policy page contains invalid authority")
		}
		lastID, lastVersion = policy.ID, policy.Version
	}
	return *page, nil
}

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
