package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/store"
)

// ProductionPrivilegePublicPage contains a frozen receipt and only the
// allowlisted public fields from one bounded page of rows.
type ProductionPrivilegePublicPage struct {
	Receipt    documentproduction.PrivilegeLogReceipt  `json:"receipt"`
	Rows       []documentproduction.PrivilegePublicRow `json:"rows"`
	NextCursor string                                  `json:"next_cursor"`
}

func registerProductionPrivilegeRoutes(api huma.API, d Deps) {
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
