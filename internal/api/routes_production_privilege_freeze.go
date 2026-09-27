package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	documentproduction "go.kenn.io/docbank/document/production"
	productionservice "go.kenn.io/docbank/internal/production"
)

type ProductionPrivilegeFreezeRequest struct {
	OperationID                      string `json:"operation_id"`
	ExpectedGeneration               int64  `json:"expected_generation" minimum:"1"`
	ExpectedInputsSHA256             string `json:"expected_inputs_sha256"`
	ExpectedApprovalEvaluationSHA256 string `json:"expected_approval_evaluation_sha256,omitzero"`
	FrozenAt                         string `json:"frozen_at"`
}

func productionPrivilegeFreezeError(err error) error {
	if problem, ok := errors.AsType[*documentproduction.Problem](err); ok {
		if problem.Code == documentproduction.ProblemApprovalRequired {
			return NewError(http.StatusConflict, "approval_required", "current approval is required")
		}
		if problem.Code == documentproduction.ProblemApprovalStale {
			return NewError(http.StatusConflict, "approval_stale", "approval no longer matches the privilege log")
		}
	}
	return productionPrivilegeMutationError(err)
}

func registerProductionPrivilegeFreezeRoutes(api huma.API, d Deps, g *OperationGate) {
	huma.Register(api, huma.Operation{
		OperationID: "freezeProductionPrivilegeLog", Method: http.MethodPost,
		Path:          "/api/v1/production-privilege-logs/{log}/revisions/{revision}/freeze",
		Summary:       "Freeze validated stored privilege rows after rechecking approval",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 4096,
	}, func(ctx context.Context, in *struct {
		Log      string `path:"log"`
		Revision int64  `path:"revision" minimum:"1"`
		Body     ProductionPrivilegeFreezeRequest
	}) (*struct {
		Body documentproduction.PrivilegeLogReceipt
	}, error) {
		if _, ok := workspaceSnapshotOwner(ctx); !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
		}
		frozenAt, err := time.Parse(time.RFC3339Nano, in.Body.FrozenAt)
		if err != nil || frozenAt.Location() != time.UTC ||
			frozenAt.Format(time.RFC3339Nano) != in.Body.FrozenAt {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_production_privilege", "frozen_at must be canonical UTC")
		}
		request := productionservice.PrivilegeLogFreezeRequest{
			OperationID: in.Body.OperationID, LogID: in.Log, Revision: in.Revision,
			ExpectedGeneration:               in.Body.ExpectedGeneration,
			ExpectedInputsSHA256:             in.Body.ExpectedInputsSHA256,
			ExpectedApprovalEvaluationSHA256: in.Body.ExpectedApprovalEvaluationSHA256,
			FrozenAt:                         frozenAt,
		}
		var receipt documentproduction.PrivilegeLogReceipt
		err = g.mutate(func() error {
			var err error
			receipt, err = productionservice.FreezeStoredPrivilegeLog(ctx, d.Store, request)
			return err
		})
		if err != nil {
			return nil, productionPrivilegeFreezeError(err)
		}
		return &struct {
			Body documentproduction.PrivilegeLogReceipt
		}{Body: receipt}, nil
	})
}
