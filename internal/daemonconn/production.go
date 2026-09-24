package daemonconn

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"uuid"
)

func (c *Connection) CreateProductionSet(ctx context.Context, request redaction.CreateRequest) (api.ProductionSetCreated, error) {
	if err := redaction.ValidateCreateRequest(request); err != nil {
		return api.ProductionSetCreated{}, err
	}
	result, err := c.API().CreateProductionSet(ctx, &apiclient.CreateProductionSetRequestOptions{Body: &request})
	if err != nil {
		return api.ProductionSetCreated{}, err
	}
	if result == nil || redaction.ValidateSet(result.Set) != nil || redaction.ValidateDraft(result.Draft) != nil ||
		result.Set.ID != result.Draft.SetID || result.Set.Name != request.Name {
		return api.ProductionSetCreated{}, integrityErrorf("production set receipt is inconsistent")
	}
	return *result, nil
}

func (c *Connection) ProductionSet(ctx context.Context, setID string) (redaction.Set, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil {
		return redaction.Set{}, err
	}
	result, err := c.API().GetProductionSet(ctx, &apiclient.GetProductionSetRequestOptions{
		PathParams: &apiclient.GetProductionSetPath{SetID: parsed}})
	if err != nil {
		return redaction.Set{}, err
	}
	if result == nil || redaction.ValidateSet(*result) != nil || result.ID != setID {
		return redaction.Set{}, integrityErrorf("production set response is inconsistent")
	}
	return *result, nil
}

func (c *Connection) ProductionDraft(ctx context.Context, setID string, revision int64) (redaction.Draft, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 {
		return redaction.Draft{}, errors.New("invalid production revision")
	}
	result, err := c.API().GetProductionDraft(ctx, &apiclient.GetProductionDraftRequestOptions{
		PathParams: &apiclient.GetProductionDraftPath{SetID: parsed, Revision: revision}})
	if err != nil {
		return redaction.Draft{}, err
	}
	if result == nil || redaction.ValidateDraft(*result) != nil || result.SetID != setID || result.Revision != revision {
		return redaction.Draft{}, integrityErrorf("production draft response is inconsistent")
	}
	return *result, nil
}

func (c *Connection) ProductionMembers(ctx context.Context, setID string, revision int64, cursor string, limit int) (api.ProductionMemberPage, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 || limit < 0 || limit > redaction.MaxProductionPage {
		return api.ProductionMemberPage{}, errors.New("invalid production member page")
	}
	query := &apiclient.ListProductionMembersQuery{}
	if cursor != "" {
		query.Cursor = &cursor
	}
	if limit != 0 {
		bounded := int64(limit)
		query.Limit = &bounded
	} else {
		limit = 100
	}
	result, err := c.API().ListProductionMembers(ctx, &apiclient.ListProductionMembersRequestOptions{
		PathParams: &apiclient.ListProductionMembersPath{SetID: parsed, Revision: revision}, Query: query})
	if err != nil {
		return api.ProductionMemberPage{}, err
	}
	if result == nil || len(result.Items) > limit {
		return api.ProductionMemberPage{}, integrityErrorf("production member page is inconsistent")
	}
	for _, item := range result.Items {
		if redaction.ValidateMember(redaction.Member(item)) != nil {
			return api.ProductionMemberPage{}, integrityErrorf("production member page contains invalid authority")
		}
	}
	return *result, nil
}

func (c *Connection) ProductionDecisions(ctx context.Context, setID string, revision int64, cursor string, limit int) (api.ProductionDecisionPage, error) {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 || limit < 0 || limit > redaction.MaxProductionPage {
		return api.ProductionDecisionPage{}, errors.New("invalid production decision page")
	}
	query := &apiclient.ListProductionDecisionsQuery{}
	if cursor != "" {
		query.Cursor = &cursor
	}
	if limit != 0 {
		bounded := int64(limit)
		query.Limit = &bounded
	} else {
		limit = 100
	}
	result, err := c.API().ListProductionDecisions(ctx, &apiclient.ListProductionDecisionsRequestOptions{
		PathParams: &apiclient.ListProductionDecisionsPath{SetID: parsed, Revision: revision}, Query: query})
	if err != nil {
		return api.ProductionDecisionPage{}, err
	}
	if result == nil || len(result.Items) > limit {
		return api.ProductionDecisionPage{}, integrityErrorf("production decision page is inconsistent")
	}
	for _, item := range result.Items {
		if redaction.ValidateDecision(redaction.Decision(item)) != nil {
			return api.ProductionDecisionPage{}, integrityErrorf("production decision page contains invalid authority")
		}
	}
	return *result, nil
}

func productionSetUUID(value string) (uuid.UUID, error) {
	if !validUUIDv4(value) {
		return uuid.UUID{}, errors.New("invalid production set ID")
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("parsing production set ID: %w", err)
	}
	return parsed, nil
}
