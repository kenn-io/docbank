package api

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func redactionProblem(ctx context.Context, err error) *Error {
	return boundedRedactionProblem(ctx, err, processing.MaxRedactionResponseBytes)
}

func boundedRedactionProblem(ctx context.Context, err error, limit int64) *Error {
	problem := classifyRedactionProblem(ctx, err)
	encoded, encodeErr := json.Marshal(problem)
	if encodeErr != nil || int64(len(encoded)) > limit {
		return NewError(422, "redaction_limit", "complete redaction diagnostics exceed the response limit; select a smaller region or review a whole-page mask")
	}
	return problem
}

func classifyRedactionProblem(ctx context.Context, err error) *Error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return NewError(504, "redaction_timeout", "native PDF request timed out")
	case errors.Is(ctx.Err(), context.Canceled):
		return NewError(408, "redaction_canceled", "native PDF request canceled")
	case errors.Is(err, processing.ErrRedactionPreparationUnavailable):
		return NewError(503, "redaction_unavailable", "native PDF operation unavailable; check the daemon and configured Linux amd64 page runtime")
	case errors.Is(err, processing.ErrRedactionPreparationBusy):
		detail := "another native PDF operation is in progress; retry after it finishes"
		if busy, ok := errors.AsType[*processing.NativePDFBusyError](err); ok {
			detail = busy.Error()
		}
		return NewError(503, "redaction_busy", detail)
	}
	if _, ok := errors.AsType[*pdfproduction.RSSLimitError](err); ok {
		return NewError(503, "redaction_memory", "daemon memory is above the renderer ceiling; retry when the daemon is quieter")
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(503, "redaction_worker_timeout", "native PDF worker timed out")
	case errors.Is(err, context.Canceled):
		return NewError(500, "redaction_failed", "native PDF worker stopped; check daemon logs")
	case errors.Is(err, store.ErrNotFound):
		return NewError(404, "not_found", "selected PDF version is unavailable")
	case errors.Is(err, processing.ErrRedactionInvalidRequest):
		return NewError(422, "invalid_redaction_request", "native PDF selection or masks are invalid")
	case errors.Is(err, processing.ErrRedactionInvalidPreparation):
		return NewError(422, "invalid_redaction_preparation", "saved native PDF preparation is inconsistent")
	case errors.Is(err, processing.ErrRedactionPreparationChanged):
		return NewError(409, "redaction_preparation_changed", "native PDF evidence changed; prepare and review again")
	case errors.Is(err, processing.ErrRedactionReviewChanged):
		return NewError(409, "redaction_review_changed", "native PDF masks or review changed; resolve and review again")
	case errors.Is(err, processing.ErrRedactionPreparationLimit):
		return NewError(422, "redaction_limit", "native PDF operation exceeds its size or work limits")
	case errors.Is(err, processing.ErrRedactionPreparationUnsupported):
		return NewError(422, "redaction_unsupported", "selected PDF cannot provide a native preparation")
	case errors.Is(err, processing.ErrRedactionPreparationIntegrity):
		return NewError(500, "redaction_integrity", "native PDF output or source failed verification")
	}
	if p, ok := errors.AsType[*redaction.Problem](err); ok {
		status, detail := 422, "redaction decision conflicts with the selected document"
		switch p.Code {
		case "source_stale":
			status, detail = 409, "selector map or page frame changed; prepare and review again"
		case "selection_expansion_required":
			detail = "review the complete proposed selection, edit masks, and resolve again"
		case "mapping_incomplete":
			detail = "mask every ordered gap; unordered gaps require a whole-page mask"
		case "invalid_decision", "decision_conflict":
		case "render_limit":
			return NewError(422, "redaction_limit", "redaction exceeds its work limits; select a smaller region or review a whole-page mask")
		default:
			return NewError(500, "redaction_failed", "native PDF resolution failed; check daemon logs")
		}
		result := NewError(status, p.Code, detail)
		result.Redaction = p
		return result
	}
	return NewError(500, "redaction_failed", "native PDF operation failed; check daemon logs")
}

// Keep this already-bounded error body out of Huma's schema-link transformer.
// The normal Error wire fields remain unchanged, and the cap covers every byte.
const redactionProblemMediaType = "application/problem+json"

type redactionWireError Error

func (e *redactionWireError) Error() string             { return e.Detail }
func (e *redactionWireError) GetStatus() int            { return e.Status }
func (e *redactionWireError) ContentType(string) string { return redactionProblemMediaType }
