package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/store"
)

// ProductionApprovalPublic contains only the allowlisted public approval
// projection. Private authority, evidence, and lifecycle reasons stay in the
// Store and never cross this transport boundary.
type ProductionApprovalPublic struct {
	Grant  documentproduction.ApprovalPublicGrant   `json:"grant"`
	Events []documentproduction.ApprovalPublicEvent `json:"events"`
}

func registerProductionApprovalRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{OperationID: "readProductionApproval", Method: http.MethodGet,
		Path: "/api/v1/production-approvals/{approval}", Summary: "Read a public production approval projection"},
		func(ctx context.Context, in *struct {
			Approval string `path:"approval"`
		}) (*struct{ Body ProductionApprovalPublic }, error) {
			grant, events, err := d.Store.ProductionApprovalPublic(ctx, in.Approval)
			if errors.Is(err, store.ErrNotFound) {
				return nil, FromStoreError(err)
			}
			if err != nil {
				return nil, NewError(http.StatusInternalServerError, "production_approval_failed", "production approval read failed")
			}
			return &struct{ Body ProductionApprovalPublic }{Body: ProductionApprovalPublic{
				Grant: grant, Events: events}}, nil
		})
}
