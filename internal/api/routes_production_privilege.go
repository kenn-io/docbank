package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	documentproduction "go.kenn.io/docbank/document/production"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

// ProductionPrivilegePublicPage contains a frozen receipt and only the
// allowlisted public fields from one bounded page of rows.
type ProductionPrivilegePublicPage struct {
	Receipt    documentproduction.PrivilegeLogReceipt  `json:"receipt"`
	Rows       []documentproduction.PrivilegePublicRow `json:"rows"`
	NextCursor string                                  `json:"next_cursor"`
}

type ProductionPrivilegeValidationRequest struct {
	OperationID        string `json:"operation_id"`
	ExpectedGeneration int64  `json:"expected_generation"`
	ValidatedAt        string `json:"validated_at"`
}

type ProductionPrivilegeValidation struct {
	DraftGeneration int64                                     `json:"draft_generation"`
	Validation      documentproduction.PrivilegeLogValidation `json:"validation"`
}

func productionPrivilegeMutationError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return FromStoreError(err)
	}
	if problem, ok := errors.AsType[*documentproduction.Problem](err); ok {
		if problem.Code == documentproduction.ProblemChangedPayload ||
			problem.Code == documentproduction.ProblemPrivilegeLogStale {
			return NewError(http.StatusConflict, "production_privilege_conflict", "privilege log authority changed")
		}
		return NewError(http.StatusUnprocessableEntity, "invalid_production_privilege", "privilege log input is invalid")
	}
	return NewError(http.StatusInternalServerError, "production_privilege_failed", "privilege log operation failed")
}

func registerProductionPrivilegeRoutes(api huma.API, d Deps, g *OperationGate) {
	huma.Register(api, huma.Operation{OperationID: "validateProductionPrivilegeLog", Method: http.MethodPost,
		Path:    "/api/v1/production-privilege-logs/{log}/revisions/{revision}/validate",
		Summary: "Validate the stored rows of a privilege-log draft", DefaultStatus: http.StatusCreated,
		MaxBodyBytes: 4096},
		func(ctx context.Context, in *struct {
			Log      string `path:"log"`
			Revision int64  `path:"revision" minimum:"1"`
			Body     ProductionPrivilegeValidationRequest
		}) (*struct{ Body ProductionPrivilegeValidation }, error) {
			if _, ok := workspaceSnapshotOwner(ctx); !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			validatedAt, err := time.Parse(time.RFC3339Nano, in.Body.ValidatedAt)
			if err != nil || validatedAt.Location() != time.UTC ||
				validatedAt.Format(time.RFC3339Nano) != in.Body.ValidatedAt {
				return nil, NewError(http.StatusUnprocessableEntity, "invalid_production_privilege", "validated_at must be canonical UTC")
			}
			request := productionservice.PrivilegeLogValidationRequest{
				OperationID: in.Body.OperationID, LogID: in.Log, Revision: in.Revision,
				ExpectedGeneration: in.Body.ExpectedGeneration, ValidatedAt: validatedAt,
			}
			var prepared productionservice.PreparedPrivilegeLogValidation
			err = g.mutate(func() error {
				var err error
				prepared, err = productionservice.ValidateStoredPrivilegeLog(ctx, d.Store, request)
				return err
			})
			if err != nil {
				return nil, productionPrivilegeMutationError(err)
			}
			return &struct{ Body ProductionPrivilegeValidation }{Body: ProductionPrivilegeValidation{
				DraftGeneration: prepared.DraftGeneration, Validation: prepared.Validation,
			}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "readProductionPrivilegeLog", Method: http.MethodGet,
		Path: "/api/v1/production-privilege-logs/{log}", Summary: "Page a frozen privilege log's public rows"},
		func(ctx context.Context, in *struct {
			Log      string `path:"log"`
			Revision int64  `query:"revision" minimum:"1"`
			Cursor   string `query:"cursor" maxLength:"6"`
			Limit    int    `query:"limit" minimum:"0" maximum:"100"`
		}) (*struct{ Body ProductionPrivilegePublicPage }, error) {
			if in.Revision < 1 {
				return nil, NewError(http.StatusUnprocessableEntity, "invalid_production_privilege_page", "revision is required")
			}
			limit := in.Limit
			if limit == 0 {
				limit = 25
			}
			page, err := d.Store.ProductionPrivilegePublicPage(ctx, in.Log, in.Revision, in.Cursor, limit)
			if errors.Is(err, store.ErrNotFound) {
				return nil, FromStoreError(err)
			}
			if problem, ok := errors.AsType[*documentproduction.Problem](err); ok &&
				problem.Code == documentproduction.ProblemInvalidContract {
				return nil, NewError(http.StatusUnprocessableEntity, "invalid_production_privilege_page", "invalid privilege log page")
			}
			if err != nil {
				return nil, NewError(http.StatusInternalServerError, "production_privilege_failed", "privilege log read failed")
			}
			return &struct{ Body ProductionPrivilegePublicPage }{Body: ProductionPrivilegePublicPage{
				Receipt: page.Receipt, Rows: page.Rows, NextCursor: page.NextCursor}}, nil
		})
}
