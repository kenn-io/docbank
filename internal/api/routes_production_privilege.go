package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	documentproduction "go.kenn.io/docbank/document/production"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

const (
	ProductionPrivilegeReceiptHashHeader = "X-Docbank-Privilege-Receipt-SHA256"
	ProductionPrivilegeRowsHashHeader    = "X-Docbank-Privilege-Rows-SHA256"
	productionPrivilegeHashPattern       = "^[0-9a-f]{64}$"
	productionPrivilegeBinaryFormat      = "binary"
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

// ProductionPrivilegeDraftCreateRequest carries private rows for one sealed
// production revision. The produced member selection is derived by storage.
type ProductionPrivilegeDraftCreateRequest struct {
	OperationID              string                            `json:"operation_id"`
	SetID                    string                            `json:"set_id"`
	SetRevision              int64                             `json:"set_revision" minimum:"1"`
	PlayersSHA256            string                            `json:"players_sha256"`
	PredecessorLogID         string                            `json:"predecessor_log_id,omitzero"`
	PredecessorReceiptSHA256 string                            `json:"predecessor_receipt_sha256,omitzero"`
	Rows                     []documentproduction.PrivilegeRow `json:"rows"`
}

// ProductionPrivilegeDraftGeneration excludes private row contents.
type ProductionPrivilegeDraftGeneration struct {
	LogID      string `json:"log_id"`
	Revision   int64  `json:"revision"`
	Generation int64  `json:"generation"`
}

// ProductionPrivilegeRowsReplaceRequest replaces all private rows at an exact
// draft generation. Storage invalidates prior validation and approval binding.
type ProductionPrivilegeRowsReplaceRequest struct {
	OperationID        string                            `json:"operation_id"`
	ExpectedGeneration int64                             `json:"expected_generation" minimum:"1"`
	Rows               []documentproduction.PrivilegeRow `json:"rows"`
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

func registerProductionPrivilegeRoutes(mux *http.ServeMux, api huma.API, d Deps, g *OperationGate) {
	mux.HandleFunc("GET /api/v1/production-privilege-logs/{log}/revisions/{revision}/exports/{format}",
		func(w http.ResponseWriter, r *http.Request) {
			revision, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
			if err != nil || revision < 1 {
				writeError(w, NewError(http.StatusUnprocessableEntity, "invalid_production_privilege_export",
					"revision must be a positive integer"))
				return
			}
			format := r.PathValue("format")
			exported, err := d.Store.ExportProductionPrivilegeLog(r.Context(), r.PathValue("log"), revision, format)
			if err != nil {
				writeEmailStoreError(w, productionPrivilegeMutationError(err))
				return
			}
			w.Header().Set("Content-Type", exported.MediaType)
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="privilege-log-%s-%d.%s"`,
				r.PathValue("log"), revision, format))
			w.Header().Set("Content-Length", strconv.Itoa(len(exported.Content)))
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set(ProductionPrivilegeReceiptHashHeader, exported.ReceiptSHA256)
			w.Header().Set(ProductionPrivilegeRowsHashHeader, exported.RowsSHA256)
			w.Header().Set("Content-Digest", contentDigest(mustDecodeHash(exported.ContentSHA256)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(exported.Content)
		})
	binary := &huma.Schema{Type: openAPIStringType, Format: productionPrivilegeBinaryFormat}
	api.OpenAPI().AddOperation(&huma.Operation{OperationID: "exportProductionPrivilegeLog", Method: http.MethodGet,
		Path:    "/api/v1/production-privilege-logs/{log}/revisions/{revision}/exports/{format}",
		Summary: "Download verified public bytes of a frozen privilege log",
		Parameters: []*huma.Param{
			{Name: "log", In: openAPIPathLocation, Required: true, Schema: &huma.Schema{Type: openAPIStringType, Format: "uuid"}},
			{Name: "revision", In: openAPIPathLocation, Required: true, Schema: &huma.Schema{Type: mailboxIntegerType, Format: "int64", Minimum: new(float64(1))}},
			{Name: "format", In: openAPIPathLocation, Required: true, Schema: &huma.Schema{Type: openAPIStringType,
				Enum: []any{"json", "csv", "xlsx", "pdf"}}},
		},
		Responses: map[string]*huma.Response{"200": {Description: "Verified public privilege-log export",
			Headers: map[string]*huma.Param{
				ProductionPrivilegeReceiptHashHeader: {Schema: &huma.Schema{Type: openAPIStringType, Pattern: productionPrivilegeHashPattern}},
				ProductionPrivilegeRowsHashHeader:    {Schema: &huma.Schema{Type: openAPIStringType, Pattern: productionPrivilegeHashPattern}},
				"Content-Digest":                     {Schema: &huma.Schema{Type: openAPIStringType}},
			},
			Content: map[string]*huma.MediaType{
				productionservice.PrivilegeLogJSONMediaType: {Schema: binary},
				productionservice.PrivilegeLogCSVMediaType:  {Schema: binary},
				productionservice.PrivilegeLogXLSXMediaType: {Schema: binary},
				productionservice.PrivilegeLogPDFMediaType:  {Schema: binary},
			}}}})
	huma.Register(api, huma.Operation{OperationID: "createProductionPrivilegeLogDraft", Method: http.MethodPost,
		Path:          "/api/v1/production-privilege-logs/{log}/revisions/{revision}/draft",
		Summary:       "Draft private privilege rows from sealed production membership",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 64 << 20},
		func(ctx context.Context, in *struct {
			Log      string `path:"log"`
			Revision int64  `path:"revision" minimum:"1"`
			Body     ProductionPrivilegeDraftCreateRequest
		}) (*struct {
			Body ProductionPrivilegeDraftGeneration
		}, error) {
			if _, ok := workspaceSnapshotOwner(ctx); !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			request := store.StoredPrivilegeLogDraftRequest{
				SetID: in.Body.SetID, SetRevision: in.Body.SetRevision,
				Draft: productionservice.PrivilegeLogDraftRequest{
					OperationID: in.Body.OperationID, LogID: in.Log, Revision: in.Revision,
					PredecessorLogID:         in.Body.PredecessorLogID,
					PredecessorReceiptSHA256: in.Body.PredecessorReceiptSHA256,
				},
				PlayersSHA256: in.Body.PlayersSHA256, Rows: in.Body.Rows,
			}
			var generation int64
			err := g.mutate(func() error {
				var err error
				generation, err = d.Store.CreateStoredPrivilegeLogDraft(ctx, request)
				return err
			})
			if err != nil {
				return nil, productionPrivilegeMutationError(err)
			}
			return &struct {
				Body ProductionPrivilegeDraftGeneration
			}{Body: ProductionPrivilegeDraftGeneration{
				LogID: in.Log, Revision: in.Revision, Generation: generation,
			}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "replaceProductionPrivilegeLogRows", Method: http.MethodPost,
		Path:         "/api/v1/production-privilege-logs/{log}/revisions/{revision}/rows",
		Summary:      "Replace private privilege rows at an exact draft generation",
		MaxBodyBytes: 64 << 20},
		func(ctx context.Context, in *struct {
			Log      string `path:"log"`
			Revision int64  `path:"revision" minimum:"1"`
			Body     ProductionPrivilegeRowsReplaceRequest
		}) (*struct {
			Body ProductionPrivilegeDraftGeneration
		}, error) {
			if _, ok := workspaceSnapshotOwner(ctx); !ok {
				return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
			}
			var generation int64
			err := g.mutate(func() error {
				var err error
				generation, err = d.Store.ReplacePrivilegeLogRows(ctx, store.PrivilegeLogRowUpdate{
					OperationID: in.Body.OperationID, LogID: in.Log, Revision: in.Revision,
					ExpectedGeneration: in.Body.ExpectedGeneration, Rows: in.Body.Rows,
				})
				return err
			})
			if err != nil {
				return nil, productionPrivilegeMutationError(err)
			}
			return &struct {
				Body ProductionPrivilegeDraftGeneration
			}{Body: ProductionPrivilegeDraftGeneration{
				LogID: in.Log, Revision: in.Revision, Generation: generation,
			}}, nil
		})
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
