package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/store"
)

// ProductionWithheldSelectionCreateRequest names the exact sealed members
// selected for withholding. The path supplies the production revision.
type ProductionWithheldSelectionCreateRequest struct {
	OperationID  string                              `json:"operation_id"`
	SelectionID  string                              `json:"selection_id"`
	PolicySHA256 string                              `json:"policy_sha256"`
	Members      []documentproduction.WithheldMember `json:"members"`
}

func productionWithheldMutationError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return FromStoreError(err)
	}
	if problem, ok := errors.AsType[*documentproduction.Problem](err); ok {
		if problem.Code == documentproduction.ProblemChangedPayload {
			return NewError(http.StatusConflict, "production_withheld_conflict", "withheld selection authority changed")
		}
		return NewError(http.StatusUnprocessableEntity, "invalid_production_withheld_selection", "withheld selection is invalid")
	}
	return NewError(http.StatusInternalServerError, "production_withheld_failed", "withheld selection operation failed")
}

func registerProductionWithheldRoutes(api huma.API, d Deps, g *OperationGate) {
	huma.Register(api, huma.Operation{
		OperationID: "createProductionWithheldSelection", Method: http.MethodPost,
		Path:          "/api/v1/productions/sets/{set_id}/revisions/{revision}/withheld-selection",
		Summary:       "Record withheld members from sealed production membership",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 64 << 20,
	}, func(ctx context.Context, in *struct {
		SetID    string `path:"set_id"`
		Revision int64  `path:"revision" minimum:"1"`
		Body     ProductionWithheldSelectionCreateRequest
	}) (*struct {
		Body documentproduction.WithheldSelection
	}, error) {
		if _, ok := workspaceSnapshotOwner(ctx); !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
		}
		selection := documentproduction.WithheldSelection{
			Contract: documentproduction.WithheldSelectionContractV1,
			ID:       in.Body.SelectionID, SetID: in.SetID, Revision: in.Revision,
			PolicySHA256: in.Body.PolicySHA256, Members: in.Body.Members,
		}
		var created documentproduction.WithheldSelection
		err := g.mutate(func() error {
			var err error
			created, err = d.Store.CreateStoredProductionWithheldSelection(ctx, in.Body.OperationID, selection)
			return err
		})
		if err != nil {
			return nil, productionWithheldMutationError(err)
		}
		return &struct {
			Body documentproduction.WithheldSelection
		}{Body: created}, nil
	})
}
