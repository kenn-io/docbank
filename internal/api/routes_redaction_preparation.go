package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func registerRedactionPreparationRoute(api huma.API, d Deps) {
	type output struct {
		CacheControl string `header:"Cache-Control"`
		Body         processing.RedactionPreparation
	}
	huma.Register(api, huma.Operation{
		OperationID: "prepareNativePDF", Method: http.MethodPost, Path: "/api/v1/redaction/prepare",
		Summary: "Inspect an exact retained native PDF for redaction preparation", MaxBodyBytes: 8 << 10,
		Errors: []int{400, 401, 403, 404, 408, 409, 413, 422, 500, 503, 504},
	}, func(ctx context.Context, input *struct {
		Body    processing.RedactionPreparationRequest
		RawBody []byte
	}) (*output, error) {
		var request processing.RedactionPreparationRequest
		if err := json.Unmarshal(input.RawBody, &request, json.RejectUnknownMembers(true)); err != nil {
			return nil, NewError(400, "invalid_redaction_preparation", "native PDF selection is invalid")
		}
		if err := processing.ValidateRedactionPreparationRequest(request); err != nil {
			if errors.Is(err, processing.ErrRedactionPreparationInvalid) {
				return nil, NewError(400, "invalid_redaction_preparation", "native PDF selection is invalid")
			}
			return nil, redactionPreparationProblem(ctx, err)
		}
		result, err := d.Processing.PrepareNativePDF(ctx, request, d.PageRuntime)
		if err != nil {
			problem := redactionPreparationProblem(ctx, err)
			if problem.Status >= 500 {
				d.Logger.ErrorContext(ctx, "preparing native PDF", "error_code", problem.Code, "error", err)
			}
			return nil, problem
		}
		return &output{CacheControl: "no-store", Body: result}, nil
	})
}

func redactionPreparationProblem(ctx context.Context, err error) *Error {
	var memory *pdfproduction.RSSLimitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return NewError(504, "redaction_preparation_timeout", "native PDF preparation request timed out")
	case errors.Is(ctx.Err(), context.Canceled):
		return NewError(408, "redaction_preparation_canceled", "native PDF preparation request was canceled")
	case errors.Is(err, processing.ErrRedactionPreparationUnavailable):
		return NewError(503, "redaction_preparation_unavailable", "native PDF preparation unavailable; check the daemon and configured Linux amd64 page runtime")
	case errors.Is(err, processing.ErrRedactionPreparationBusy):
		return NewError(503, "redaction_preparation_busy", "another native PDF preparation is active; retry later")
	case errors.As(err, &memory):
		return NewError(503, "redaction_preparation_memory", "daemon memory is above the renderer ceiling; retry when the daemon is quieter")
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(503, "redaction_preparation_worker_timeout", "native PDF inspection worker timed out")
	case errors.Is(err, store.ErrNotFound):
		return NewError(404, "not_found", "selected PDF version is unavailable")
	case errors.Is(err, store.ErrStaleRevision):
		return NewError(409, "redaction_selection_changed", "selected document changed; inspect and select it again")
	case errors.Is(err, processing.ErrRedactionPreparationInvalid):
		return NewError(422, "invalid_redaction_preparation", "native PDF selection is invalid")
	case errors.Is(err, processing.ErrRedactionPreparationLimit):
		return NewError(422, "redaction_preparation_limit", "native PDF preparation exceeds its size or work limits")
	case errors.Is(err, processing.ErrRedactionPreparationUnsupported):
		return NewError(422, "redaction_preparation_unsupported", "selected PDF cannot provide a native preparation")
	case errors.Is(err, processing.ErrRedactionPreparationIntegrity):
		return NewError(500, "redaction_preparation_integrity", "native PDF preparation failed verification")
	default:
		return NewError(500, "redaction_preparation_failed", "native PDF preparation failed; check daemon logs")
	}
}
