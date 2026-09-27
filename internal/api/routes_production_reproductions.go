package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

func productionReproductionError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return FromStoreError(err)
	}
	if errors.Is(err, production.ErrReproductionConflict) {
		return NewError(http.StatusConflict, "production_reproduction_conflict",
			"reproduction receipt disagrees with the original production or retained package")
	}
	return NewError(http.StatusInternalServerError, "production_reproduction_failed",
		"reproduction receipt could not be verified")
}

func registerProductionReproductionRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "getProductionReproduction", Method: http.MethodGet,
		Path:    "/api/v1/productions/jobs/{job_id}/reproductions/{operation_id}",
		Summary: "Read an exact verified reproduction receipt and original production link",
	}, func(ctx context.Context, in *struct {
		JobID       string `path:"job_id" format:"uuid"`
		OperationID string `path:"operation_id" format:"uuid"`
	}) (*struct {
		Body documentproduction.ReproductionReceipt
	}, error) {
		if _, ok := workspaceSnapshotOwner(ctx); !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
		}
		receipt, err := d.Store.LoadProductionReproduction(ctx, in.JobID, in.OperationID)
		if err != nil {
			return nil, productionReproductionError(err)
		}
		return &struct {
			Body documentproduction.ReproductionReceipt
		}{Body: receipt}, nil
	})
}
