package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	documentproduction "go.kenn.io/docbank/document/production"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

type ProductionPolicyCreateRequest struct {
	OperationID string                           `json:"operation_id"`
	Policy      documentproduction.PolicyVersion `json:"policy"`
}

type ProductionPolicyPage struct {
	Items      []documentproduction.PolicyVersion `json:"items"`
	NextCursor string                             `json:"next_cursor"`
}

func productionPolicyError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return FromStoreError(err)
	}
	if problem, ok := errors.AsType[*documentproduction.Problem](err); ok {
		if problem.Code == documentproduction.ProblemChangedPayload {
			return NewError(http.StatusConflict, "production_policy_conflict", "policy version or operation ID names different input")
		}
		return NewError(http.StatusUnprocessableEntity, "invalid_production_policy", "production policy input is invalid")
	}
	return NewError(http.StatusInternalServerError, "production_policy_failed", "production policy operation failed")
}

func registerProductionPolicyRoutes(api huma.API, d Deps, g *OperationGate) {
	huma.Register(api, huma.Operation{OperationID: "listProductionPolicyVersions", Method: http.MethodGet,
		Path: "/api/v1/productions/policies", Summary: "Page immutable production policy versions"},
		func(ctx context.Context, in *struct {
			Cursor string `query:"cursor" maxLength:"60"`
			Limit  int    `query:"limit" minimum:"0" maximum:"100"`
		}) (*struct{ Body ProductionPolicyPage }, error) {
			limit := in.Limit
			if limit == 0 {
				limit = 25
			}
			page, err := d.Store.ListProductionPolicies(ctx, in.Cursor, limit)
			if err != nil {
				return nil, productionPolicyError(err)
			}
			return &struct{ Body ProductionPolicyPage }{Body: ProductionPolicyPage{
				Items: page.Items, NextCursor: page.NextCursor}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "createProductionPolicyVersion", Method: http.MethodPost,
		Path: "/api/v1/productions/policies", Summary: "Store an immutable production policy version",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 1 << 20},
		func(ctx context.Context, in *struct{ Body ProductionPolicyCreateRequest }) (*struct {
			Body documentproduction.PolicyVersion
		}, error) {
			if _, ok := workspaceSnapshotOwner(ctx); !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			prepared, err := productionservice.PreparePolicyVersion(in.Body.OperationID, in.Body.Policy)
			if err != nil {
				return nil, productionPolicyError(err)
			}
			var policy documentproduction.PolicyVersion
			err = g.mutate(func() error {
				var err error
				policy, err = d.Store.PutProductionPolicy(ctx, prepared)
				return err
			})
			if err != nil {
				return nil, productionPolicyError(err)
			}
			return &struct {
				Body documentproduction.PolicyVersion
			}{Body: policy}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "readProductionPolicyVersion", Method: http.MethodGet,
		Path:    "/api/v1/productions/policies/{policy_id}/versions/{version}",
		Summary: "Read an exact immutable production policy version"},
		func(ctx context.Context, in *struct {
			PolicyID string `path:"policy_id"`
			Version  int64  `path:"version" minimum:"1"`
		}) (*struct {
			Body documentproduction.PolicyVersion
		}, error) {
			policy, err := d.Store.ProductionPolicy(ctx, in.PolicyID, in.Version)
			if err != nil {
				return nil, productionPolicyError(err)
			}
			return &struct {
				Body documentproduction.PolicyVersion
			}{Body: policy}, nil
		})
}
