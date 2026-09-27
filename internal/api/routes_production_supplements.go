package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

// ProductionSupplementRequest keeps the OpenAPI client imports unambiguous
// while preserving the supplement service's canonical JSON contract.
type ProductionSupplementRequest production.SupplementRequest

// ProductionSupplementRecord is the exact immutable supplement wire record.
type ProductionSupplementRecord production.SupplementRecord

func productionSupplementError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return FromStoreError(err)
	}
	if errors.Is(err, production.ErrSupplementConflict) {
		return NewError(http.StatusConflict, "production_supplement_conflict",
			"supplement inputs no longer match the published parent and prepared child")
	}
	return NewError(http.StatusInternalServerError, "production_supplement_failed",
		"supplement operation failed")
}

func registerProductionSupplementRoutes(api huma.API, d Deps, g *OperationGate) {
	huma.Register(api, huma.Operation{
		OperationID: "createProductionSupplement", Method: http.MethodPost,
		Path:          "/api/v1/productions/supplements",
		Summary:       "Reserve continuation numbers and record an exact parent-child production link",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 4096,
	}, func(ctx context.Context, in *struct {
		Body ProductionSupplementRequest
	}) (*struct{ Body ProductionSupplementRecord }, error) {
		actor, ok := workspaceSnapshotOwner(ctx)
		if !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
		}
		var record production.SupplementRecord
		err := g.mutate(func() error {
			var err error
			record, err = d.Store.CreateProductionSupplement(ctx, actor, production.SupplementRequest(in.Body))
			return err
		})
		if err != nil {
			return nil, productionSupplementError(err)
		}
		return &struct{ Body ProductionSupplementRecord }{Body: ProductionSupplementRecord(record)}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "getProductionSupplement", Method: http.MethodGet,
		Path:    "/api/v1/productions/supplements/{operation_id}",
		Summary: "Read an exact verified production supplement record",
	}, func(ctx context.Context, in *struct {
		OperationID string `path:"operation_id" format:"uuid"`
	}) (*struct{ Body ProductionSupplementRecord }, error) {
		if _, ok := workspaceSnapshotOwner(ctx); !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
		}
		record, err := d.Store.LoadProductionSupplement(ctx, in.OperationID)
		if err != nil {
			return nil, productionSupplementError(err)
		}
		return &struct{ Body ProductionSupplementRecord }{Body: ProductionSupplementRecord(record)}, nil
	})
}
