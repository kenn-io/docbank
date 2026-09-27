package daemonconn

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// ProductionApproval reads the public grant and lifecycle projection. The
// private subject, authority, evidence, and event reasons never enter this type.
func (c *Connection) ProductionApproval(ctx context.Context, approvalID string) (api.ProductionApprovalPublic, error) {
	if approvalID == "" {
		return api.ProductionApprovalPublic{}, errors.New("invalid production approval reference")
	}
	result, err := c.API().ReadProductionApproval(ctx,
		&apiclient.ReadProductionApprovalRequestOptions{PathParams: &apiclient.ReadProductionApprovalPath{
			Approval: approvalID}})
	if err != nil {
		return api.ProductionApprovalPublic{}, err
	}
	if result == nil || result.Grant.ID != approvalID {
		return api.ProductionApprovalPublic{}, integrityErrorf("production approval read is inconsistent")
	}
	for _, event := range result.Events {
		if event.ApprovalID != approvalID {
			return api.ProductionApprovalPublic{}, integrityErrorf("production approval event targets another grant")
		}
	}
	return *result, nil
}
